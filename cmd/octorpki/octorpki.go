package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/asn1"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	syncpki "github.com/cloudflare/cfrpki/sync/lib"
	librpki "github.com/cloudflare/cfrpki/validator/lib"
	"github.com/cloudflare/cfrpki/validator/pki"

	"github.com/rs/cors"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/cloudflare/gortr/prefixfile"
	log "github.com/sirupsen/logrus"

	// Debugging
	"net/http/pprof"

	"github.com/getsentry/sentry-go"
	"github.com/opentracing/opentracing-go"
	jcfg "github.com/uber/jaeger-client-go/config"
)

var (
	version    = ""
	buildinfos = ""
	AppVersion = "OctoRPKI " + version + " " + buildinfos
	AllowRoot  = flag.Bool("allow.root", false, "Allow starting as root")

	// Validator Options
	RootTAL       = flag.String("tal.root", "tals/afrinic.tal,tals/apnic.tal,tals/arin.tal,tals/lacnic.tal,tals/ripe.tal", "List of TAL separated by comma")
	TALNames      = flag.String("tal.name", "AFRINIC,APNIC,ARIN,LACNIC,RIPE", "Name of the TALs")
	UseManifest   = flag.Bool("manifest.use", true, "Use manifests file to explore instead of going into the repository")
	Basepath      = flag.String("cache", "cache/", "Base directory to store certificates")
	LogLevel      = flag.String("loglevel", "info", "Log level")
	Refresh       = flag.String("refresh", "20m", "Revalidation interval")
	MaxIterations = flag.Int("max.iterations", 32, "Specify the max number of iterations octorpki will make before failing to generate output.json")

	StrictManifests = flag.Bool("strict.manifests", true, "Manifests must be complete or invalidate CA")
	StrictHash      = flag.Bool("strict.hash", true, "Check the hash of files")
	StrictCms       = flag.Bool("strict.cms", false, "Decode CMS with strict settings")

	// Rsync Options
	RsyncTimeout = flag.Duration("rsync.timeout", time.Minute*20, "Rsync command timeout")
	RsyncBin     = flag.String("rsync.bin", DefaultBin(), "The rsync binary to use")

	// RRDP Options
	RRDP         = flag.Bool("rrdp", true, "Enable RRDP fetching")
	RRDPFile     = flag.String("rrdp.file", "cache/rrdp.json", "Save RRDP state")
	RRDPFailover = flag.Bool("rrdp.failover", true, "Failover to rsync when RRDP fails")
	UserAgent    = flag.String("useragent", fmt.Sprintf("Cloudflare-RRDP-%v (+https://github.com/cloudflare/cfrpki)", AppVersion), "User-Agent header")

	Mode       = flag.String("mode", "server", "Select output mode (server/oneoff)")
	WaitStable = flag.Bool("output.wait", true, "Wait until stable state to create the file (returns 503 when unstable on HTTP)")

	// Serving Options
	Addr        = flag.String("http.addr", ":8081", "Listening address")
	CacheHeader = flag.Bool("http.cache", true, "Enable cache header")
	MetricsPath = flag.String("http.metrics", "/metrics", "Prometheus metrics endpoint")
	InfoPath    = flag.String("http.info", "/infos", "Information URL")
	HealthPath  = flag.String("http.health", "/health", "Health URL")

	CorsOrigins = flag.String("cors.origins", "*", "Cors origins separated by comma")
	CorsCreds   = flag.Bool("cors.creds", false, "Cors enable credentials")

	// File option
	Output           = flag.String("output.roa", "output.json", "Output ROA file or URL")
	Sign             = flag.Bool("output.sign", true, "Sign output (GoRTR compatible)")
	SignKey          = flag.String("output.sign.key", "private.pem", "ECDSA signing key")
	ValidityDuration = flag.Duration("output.sign.validity", time.Hour, "Validity")

	// Debugging options
	Pprof     = flag.Bool("pprof", false, "Enable pprof endpoint")
	Tracer    = flag.Bool("tracer", false, "Enable tracer")
	SentryDSN = flag.String("sentry.dsn", "", "Send errors to Sentry")

	Version = flag.Bool("version", false, "Print version")

	CertRepository = asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 7, 48, 5}
	CertRRDP       = asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 7, 48, 13}

	MetricSIACounts = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "file_count_sia",
			Help: "Counts of file per SIA.",
		},
		[]string{"address", "type"},
	)
	MetricRsyncErrors = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "rsync_errors",
			Help: "Rsync error count.",
		},
		[]string{"address"},
	)
	MetricRRDPErrors = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "rrdp_errors",
			Help: "RRDP error count.",
		},
		[]string{"address"},
	)
	MetricRRDPSerial = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "rrdp_serial",
			Help: "RRDP serial number.",
		},
		[]string{"address"},
	)
	MetricROAsCount = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "roas",
			Help: "Bytes received by the application.",
		},
		[]string{"ta"},
	)
	MetricState = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "state",
			Help: "State of the Relying party (1 = stable, 0 = unstable).",
		},
	)
	MetricLastStableValidation = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "last_stable_validation",
			Help: "Timestamp of last stable validation.",
		},
	)
	MetricLastValidation = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "last_validation",
			Help: "Timestamp of last validation.",
		},
	)
	MetricOperationTime = prometheus.NewSummaryVec(
		prometheus.SummaryOpts{
			Name:       "operation_time",
			Help:       "Time to run an operation.",
			Objectives: map[float64]float64{0.5: 0.05, 0.9: 0.01, 0.99: 0.001},
		},
		[]string{"type"},
	)
	MetricLastFetch = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "last_fetch",
			Help: "RRDP/Rsync last timestamp.",
		},
		[]string{"address", "type"},
	)
)

func DefaultBin() string {
	path, _ := exec.LookPath("rsync")
	return path
}

type RRDPInfo struct {
	Rsync     string `json:"rsync"`
	Path      string `json:"path"`
	SessionID string `json:"sessionid"`
	Serial    int64  `json:"serial"`
}

var errKeyNotParsed = fmt.Errorf("Failed to PEM decode key")

func ReadKey(key []byte, isPem bool) (*ecdsa.PrivateKey, error) {
	if isPem {
		block, _ := pem.Decode(key)
		if block == nil {
			return nil, errKeyNotParsed
		}
		key = block.Bytes
	}

	k, err := x509.ParseECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	return k, nil
}

type Stats struct {
	URI       string  `json:"uri"`
	Count     int     `json:"file-count"`
	Iteration int     `json:"iteration"`
	Errors    int     `json:"errors"`
	Duration  float64 `json:"duration"`

	LastFetch      int `json:"last-fetch"`
	LastFetchError int `json:"last-fetch-error,omitempty"`

	RRDPSerial    int64  `json:"rrdp-serial,omitempty"`
	RRDPSessionID string `json:"rrdp-sessionid,omitempty"`
	RRDPLastFile  string `json:"rrdp-last-file,omitempty"`

	LastError string `json:"last-error,omitempty"`
}

type OctoRPKI struct {
	Basepath    string
	Tals        []*pki.PKIFile
	TalsFetch   map[string]*librpki.RPKITAL
	TalNames    []string
	UseManifest bool

	Mode string

	LastComputed time.Time
	WaitStable   bool
	Sign         bool
	Key          *ecdsa.PrivateKey

	Stable            bool // Indicates something has been added to the fetch list (rsync or rrdp)
	HasPreviousStable bool
	Fetcher           *syncpki.LocalFetch
	HTTPFetcher       *syncpki.HTTPFetcher

	PrevRepos    map[string]time.Time
	CurrentRepos map[string]time.Time

	RsyncFetch      map[string]string
	RRDPFetch       map[string]string
	RRDPFetchDomain map[string]string

	RRDPInfo     map[string]RRDPInfo
	RRDPFailover bool

	ROAList     *prefixfile.ROAList
	ROAListLock *sync.RWMutex

	// Various counters and statistics
	RRDPStats          map[string]*Stats
	RsyncStats         map[string]Stats
	CountExplore       int
	ValidationDuration time.Duration
	Iteration          int
	ValidationMessages []string
	ROAsTALsCount      []ROAsTAL

	InfoAuthorities     [][]SIA
	InfoAuthoritiesLock *sync.RWMutex

	StrictHash      bool
	StrictManifests bool
	StrictCms       bool
}

func (s *OctoRPKI) MainReduce() bool {
	var hasChanged bool
	for rsync, ts := range s.CurrentRepos {
		if _, ok := s.PrevRepos[rsync]; !ok {
			s.PrevRepos[rsync] = ts
			hasChanged = true
			log.Debugf("Repository %s has appeared at %v", rsync, ts)
		}
	}

	// Init deletion of folder if missing from current

	s.Fetcher.SetRepositories(s.CurrentRepos)

	if len(s.PrevRepos) != len(s.CurrentRepos) {
		return true
	}

	return hasChanged
}

func ExtractRsyncDomain(rsync string) (string, error) {
	if len(rsync) > len("rsync://") {
		rsyncDomain := strings.Split(rsync[8:], "/")
		return "rsync://" + rsyncDomain[0], nil
	} else {
		return "", errors.New("Wrong size")
	}
}

func (s *OctoRPKI) WriteRsyncFileOnDisk(path string, data []byte) error {
	fPath, err := syncpki.GetDownloadPath(path, true)
	if err != nil {
		log.Fatal(err)
	}

	err = os.MkdirAll(filepath.Join(s.Basepath, fPath), os.ModePerm)
	if err != nil {
		log.Fatal(err)
	}

	fPath, err = syncpki.GetDownloadPath(path, false)
	if err != nil {
		log.Fatal(err)
	}

	// GHSA-8459-6rc9-8vf8: Prevent parent directory writes outside of Basepath
	if strings.Contains(fPath, "../") || strings.Contains(fPath, "..\\") {
		return fmt.Errorf("Path %q contains illegal path element", fPath)
	}

	fp := filepath.Join(s.Basepath, fPath)
	err = ioutil.WriteFile(fp, data, 0600)
	if err != nil {
		return fmt.Errorf("Unable to write file %q: %v", fp, err)
	}

	return nil
}

func (s *OctoRPKI) ReceiveRRDPFileCallback(main string, url string, path string, data []byte, withdraw bool, snapshot bool, serial int64, args ...interface{}) error {
	if len(args) > 0 {
		rsync, ok := args[0].(string)
		if ok && !strings.Contains(path, rsync) {
			log.Errorf("rrdp: %s is outside directory %s", path, rsync)
			return nil
		}
	}

	err := s.WriteRsyncFileOnDisk(path, data)
	if err != nil {
		return err
	}

	MetricSIACounts.With(
		prometheus.Labels{
			"address": main,
			"type":    "rrdp",
		}).Inc()

	s.RRDPStats[main].Count++
	s.RRDPStats[main].RRDPLastFile = url
	return nil
}

func (s *OctoRPKI) LoadRRDP(file string) error {
	fc, err := ioutil.ReadFile(file)
	if err != nil {
		return fmt.Errorf("Unable to read file %q: %v", file, err)
	}

	s.RRDPInfo = make(map[string]RRDPInfo)
	err = json.Unmarshal(fc, &s.RRDPInfo)
	if err != nil {
		return fmt.Errorf("JSON unmarshal failed: %v", err)
	}

	return nil
}

func (s *OctoRPKI) SaveRRDP(file string) error {
	fc, err := json.Marshal(file)
	if err != nil {
		return fmt.Errorf("JSON marshal failed: %v", err)
	}

	err = ioutil.WriteFile(file, fc, 0600)
	if err != nil {
		return fmt.Errorf("Unable to write file %q: %v", file, err)
	}

	return nil
}

func (s *OctoRPKI) MainRRDP(pSpan opentracing.Span) {
	tracer := opentracing.GlobalTracer()
	span := tracer.StartSpan(
		"rrdp",
		opentracing.ChildOf(pSpan.Context()),
	)
	defer span.Finish()

	for path, rsync := range s.RRDPFetch {
		rSpan := tracer.StartSpan(
			"sync",
			opentracing.ChildOf(span.Context()),
		)
		rSpan.SetTag("rrdp", path)
		rSpan.SetTag("rsync", rsync)
		rSpan.SetTag("type", "rrdp")
		log.Infof("RRDP sync %v", path)

		info := s.RRDPInfo[rsync]

		MetricSIACounts.With(
			prometheus.Labels{
				"address": path,
				"type":    "rrdp",
			}).Set(0)

		if _, exists := s.RRDPStats[path]; !exists {
			s.RRDPStats[path] = &Stats{}
		}

		s.RRDPStats[path].URI = path
		s.RRDPStats[path].Iteration++
		s.RRDPStats[path].Count = 0

		t1 := time.Now().UTC()

		rrdp := &syncpki.RRDPSystem{
			Callback: s.ReceiveRRDPFileCallback,

			Path:    path,
			Fetcher: s.HTTPFetcher,

			SessionID: info.SessionID,
			Serial:    info.Serial,

			Log: log.StandardLogger(),
		}
		err := rrdp.FetchRRDP(s.RRDPFetchDomain[path])
		t2 := time.Now().UTC()
		if err != nil {
			rSpan.SetTag("error", true)

			sentry.WithScope(func(scope *sentry.Scope) {
				if errC, ok := err.(interface{ SetURL(string, string) }); ok {
					errC.SetURL(path, rsync)
				}
				if errC, ok := err.(interface{ SetSentryScope(*sentry.Scope) }); ok {
					errC.SetSentryScope(scope)
				}
				rrdp.SetSentryScope(scope)
				scope.SetTag("Rsync", rsync)
				scope.SetTag("RRDP", path)
				sentry.CaptureException(err)
			})

			// GHSA-g9wh-3vrx-r7hg: Do not process responses that are too large
			if s.RRDPFailover && err.Error() != "http: request body too large" {
				log.Errorf("Error when processing %v (for %v): %v. Will add to rsync.", path, rsync, err)
				rSpan.LogKV("event", "rrdp failure", "type", "failover to rsync", "message", err)
			} else {
				log.Errorf("Error when processing %v (for %v): %v.Skipping failover to rsync.", path, rsync, err)
				rSpan.LogKV("event", "rrdp failure", "type", "skipping failover to rsync", "message", err)
				delete(s.RsyncFetch, rsync)
			}

			MetricRRDPErrors.With(
				prometheus.Labels{
					"address": path,
				}).Inc()

			s.RRDPStats[path].Errors++
			s.RRDPStats[path].LastFetchError = int(time.Now().Unix())
			s.RRDPStats[path].LastError = err.Error()
			s.RRDPStats[path].Duration = t2.Sub(t1).Seconds()

			rSpan.Finish()
			continue
		} else {
			log.Debugf("Success fetching %s, removing rsync %s", path, rsync)
			delete(s.RsyncFetch, rsync)
		}

		rSpan.LogKV("event", "rrdp", "type", "success", "message", "rrdp successfully fetched")
		sentry.WithScope(func(scope *sentry.Scope) {
			scope.SetLevel(sentry.LevelInfo)
			scope.SetTag("Rsync", rsync)
			scope.SetTag("RRDP", path)
			rrdp.SetSentryScope(scope)
			sentry.CaptureMessage("fetched rrdp successfully")
		})

		rSpan.Finish()
		MetricRRDPSerial.With(
			prometheus.Labels{
				"address": path,
			}).Set(float64(rrdp.Serial))
		lastFetch := time.Now().UTC().UnixNano() / 1000000000
		MetricLastFetch.With(
			prometheus.Labels{
				"address": path,
				"type":    "rrdp",
			}).Set(float64(lastFetch))

		s.RRDPStats[path].LastFetch = int(lastFetch)
		s.RRDPStats[path].RRDPSerial = rrdp.Serial
		s.RRDPStats[path].RRDPSessionID = rrdp.SessionID
		s.RRDPStats[path].Duration = t2.Sub(t1).Seconds()

		s.RRDPInfo[rsync] = RRDPInfo{
			Rsync:     rsync,
			Path:      path,
			SessionID: rrdp.SessionID,
			Serial:    rrdp.Serial,
		}
	}
}

func (s *OctoRPKI) MainRsync(pSpan opentracing.Span) {
	tracer := opentracing.GlobalTracer()
	span := tracer.StartSpan(
		"rsync",
		opentracing.ChildOf(pSpan.Context()),
	)
	defer span.Finish()

	rsync := syncpki.RsyncSystem{
		Log: log.StandardLogger(),
	}

	for uri, _ := range s.RsyncFetch {
		rSpan := tracer.StartSpan(
			"sync",
			opentracing.ChildOf(span.Context()),
		)
		rSpan.SetTag("rsync", uri)
		rSpan.SetTag("type", "rsync")

		log.Infof("Rsync sync %v", uri)
		downloadPath, err := syncpki.GetDownloadPath(uri, true)
		if err != nil {
			log.Fatal(err)
		}

		tmpStats := s.RsyncStats[uri]
		tmpStats.URI = uri
		tmpStats.Iteration++
		tmpStats.Count = 0
		s.RsyncStats[uri] = tmpStats

		path := filepath.Join(s.Basepath, downloadPath)
		ctxRsync, cancelRsync := context.WithTimeout(context.Background(), *RsyncTimeout)
		t1 := time.Now().UTC()
		files, err := rsync.RunRsync(ctxRsync, uri, *RsyncBin, path)
		t2 := time.Now().UTC()
		if err != nil {
			rSpan.SetTag("error", true)
			rSpan.LogKV("event", "rsync failure", "message", err)
			log.Errorf("Error when processing %v (for %v): %v. Will add to rsync.", path, rsync, err)
			sentry.WithScope(func(scope *sentry.Scope) {
				if errC, ok := err.(interface{ SetRsync(string) }); ok {
					errC.SetRsync(uri)
				}
				if errC, ok := err.(interface{ SetSentryScope(*sentry.Scope) }); ok {
					errC.SetSentryScope(scope)
				}
				scope.SetTag("Rsync", uri)
				sentry.CaptureException(err)
			})

			MetricRsyncErrors.With(
				prometheus.Labels{
					"address": uri,
				}).Inc()

			tmpStats = s.RsyncStats[uri]
			tmpStats.Errors++
			tmpStats.LastFetchError = int(time.Now().UTC().UnixNano() / 1000000000)
			tmpStats.LastError = fmt.Sprint(err)
			s.RsyncStats[uri] = tmpStats
		} else {
			rSpan.LogKV("event", "rsync", "type", "success", "message", "rsync successfully fetched")
			sentry.WithScope(func(scope *sentry.Scope) {
				scope.SetLevel(sentry.LevelInfo)
				scope.SetTag("Rsync", uri)
				sentry.CaptureMessage("fetched rsync successfully")
			})
		}
		cancelRsync()
		var countFiles int
		if files != nil {
			countFiles = len(files)
		}

		rSpan.Finish()

		MetricSIACounts.With(
			prometheus.Labels{
				"address": uri,
				"type":    "rsync",
			}).Set(float64(countFiles))
		lastFetch := time.Now().UTC().UnixNano() / 1000000000
		MetricLastFetch.With(
			prometheus.Labels{
				"address": uri,
				"type":    "rsync",
			}).Set(float64(lastFetch))
		tmpStats = s.RsyncStats[uri]
		tmpStats.LastFetch = int(lastFetch)
		tmpStats.Count = countFiles
		tmpStats.Duration = t2.Sub(t1).Seconds()
		s.RsyncStats[uri] = tmpStats
	}
}

func (s *OctoRPKI) Debugf(msg string, args ...interface{}) {
	log.Debugf(msg, args...)
}

func (s *OctoRPKI) Errorf(msg string, args ...interface{}) {
	log.Errorf(msg, args...)
	s.ValidationMessages = append(s.ValidationMessages, fmt.Sprintf(msg, args...))
}

func (s *OctoRPKI) Printf(msg string, args ...interface{}) {
	log.Printf(msg, args...)
	s.ValidationMessages = append(s.ValidationMessages, fmt.Sprintf(msg, args...))
}

func (s *OctoRPKI) Warnf(msg string, args ...interface{}) {
	log.Warnf(msg, args...)
	s.ValidationMessages = append(s.ValidationMessages, fmt.Sprintf(msg, args...))
}

func FilterDuplicates(roalist []prefixfile.ROAJson) []prefixfile.ROAJson {
	roalistNodup := make([]prefixfile.ROAJson, 0)
	hmap := make(map[string]bool)
	for _, roa := range roalist {
		k := roa.String()
		_, present := hmap[k]
		if !present {
			hmap[k] = true
			roalistNodup = append(roalistNodup, roa)
		}
	}
	return roalistNodup
}

func setJaegerError(l []interface{}, err error) []interface{} {
	return append(l, "error", true, "message", err)
}

// Fetches RFC8630-type TAL
func (s *OctoRPKI) MainTAL(pSpan opentracing.Span) {
	tracer := opentracing.GlobalTracer()
	span := tracer.StartSpan(
		"tal",
		opentracing.ChildOf(pSpan.Context()),
	)

	for path, tal := range s.TalsFetch {
		tSpan := tracer.StartSpan(
			"tal-fetch",
			opentracing.ChildOf(span.Context()),
		)
		tSpan.SetTag("tal", path)

		// Try the multiple URLs a TAL can be hosted on
		var success bool
		var successUrl string

		sHub := sentry.CurrentHub().Clone()

		for _, uri := range tal.URI {
			if strings.HasPrefix(uri, "http://") || strings.HasPrefix(uri, "https://") {

				tfSpan := tracer.StartSpan(
					"tal-fetch-uri",
					opentracing.ChildOf(tSpan.Context()),
				)
				tfSpan.SetTag("uri", uri)
				//tLogs := []interface{}{"event", "fetch tal", "uri", uri}

				sHub.ConfigureScope(func(scope *sentry.Scope) {
					scope.SetTag("tal.uri", uri)
					scope.SetTag("tal.path", path)
				})

				req, err := http.NewRequest("GET", uri, nil)
				if err != nil {
					tfSpan.SetTag("error", true)
					tfSpan.SetTag("message", err)
					tfSpan.Finish()
					log.Errorf("error while trying to fetch: %s: %v", uri, err)
					continue
				}
				req.Header.Set("User-Agent", s.HTTPFetcher.UserAgent)

				sHub.ConfigureScope(func(scope *sentry.Scope) {
					scope.SetRequest(req)
				})

				sbc := &sentry.Breadcrumb{
					Message:  fmt.Sprintf("GET | %s", uri),
					Category: "http",
				}

				// maybe add a limit in the client? To avoid downloading huge files (that wouldn't be certs)
				resp, err := s.HTTPFetcher.Client.Do(req)
				if err != nil {
					tfSpan.SetTag("error", true)
					tfSpan.SetTag("message", err)
					//tSpan.LogKV(setJaegerError(tLogs, err)...)
					tfSpan.Finish()

					sbc.Level = sentry.LevelError
					sHub.AddBreadcrumb(sbc, nil)
					log.Errorf("error while trying to fetch: %s: %v", uri, err)
					sHub.CaptureException(err)
					continue
				}

				if resp.StatusCode != 200 {
					msg := fmt.Sprintf("http server replied: %s", resp.Status)

					tfSpan.SetTag("error", true)
					tfSpan.SetTag("message", msg)
					tfSpan.Finish()

					sHub.ConfigureScope(func(scope *sentry.Scope) {
						scope.SetLevel(sentry.LevelError)
					})
					sbc.Level = sentry.LevelError
					sHub.AddBreadcrumb(sbc, nil)

					log.Errorf("http server replied: %s while trying to fetch %s", resp.Status, uri)
					sHub.CaptureMessage(msg)
					continue
				}

				sHub.AddBreadcrumb(sbc, nil)

				// check body / status code
				data, err := ioutil.ReadAll(resp.Body)
				tfSpan.LogKV("size", len(data))
				if err != nil {
					tfSpan.SetTag("error", true)
					tfSpan.SetTag("message", err)
					tfSpan.Finish()

					log.Errorf("error while trying to fetch: %s: %v", uri, err)
					sHub.CaptureException(err)
					continue
				}

				// Plan option to store everything in memory
				err = s.WriteRsyncFileOnDisk(tal.GetRsyncURI(), data)
				if err != nil {
					tfSpan.SetTag("error", true)
					tfSpan.SetTag("message", err)
					tfSpan.Finish()

					log.Errorf("error while trying to fetch: %s: %v", uri, err)
					sHub.CaptureException(err)
					continue
				}

				//tSpan.LogKV(append(tLogs, "success", true)...)
				tfSpan.Finish()

				sHub.WithScope(func(scope *sentry.Scope) {
					scope.SetLevel(sentry.LevelInfo)
					sHub.CaptureMessage("fetched http tal cert successfully")
				})

				success = true
				successUrl = uri
				break

			}
		}

		// Fail over to rsync
		if !success && s.RRDPFailover && tal.HasRsync() {
			rsync := tal.GetRsyncURI()
			log.Infof("Root certificate for %s will be downloaded using rsync: %s", path, rsync)
			s.RsyncFetch[rsync] = ""
			tSpan.SetTag("failover-rsync", true)
		} else if success {
			log.Infof("Successfully downloaded root certificate for %s at %s", path, successUrl)
		} else {
			log.Errorf("Could not download root certificate for %s", path)
			tSpan.SetTag("error", true)
		}

		tSpan.Finish()
	}

	defer span.Finish()
}

func (s *OctoRPKI) MainValidation(pSpan opentracing.Span) {
	ia := make([][]SIA, len(s.Tals))
	for i := 0; i < len(ia); i++ {
		ia[i] = make([]SIA, 0)
	}
	iatmp := make(map[string]*SIA)

	tracer := opentracing.GlobalTracer()
	span := tracer.StartSpan(
		"validation",
		opentracing.ChildOf(pSpan.Context()),
	)
	defer span.Finish()

	manager := make([]*pki.SimpleManager, len(s.Tals))
	for i, tal := range s.Tals {
		tSpan := tracer.StartSpan(
			"explore",
			opentracing.ChildOf(span.Context()),
		)
		tSpan.SetTag("tal", tal.Path)

		validator := pki.NewValidator()
		validator.DecoderConfig.ValidateStrict = s.StrictCms

		sm := pki.NewSimpleManager()
		manager[i] = sm
		manager[i].ReportErrors = true
		manager[i].Validator = validator
		manager[i].FileSeeker = s.Fetcher
		manager[i].Log = s
		manager[i].StrictHash = s.StrictHash
		manager[i].StrictManifests = s.StrictManifests

		go func(sm *pki.SimpleManager, tal *pki.PKIFile) {
			for err := range sm.Errors {
				tSpan.SetTag("error", true)
				tSpan.LogKV("event", "resource issue", "type", "skipping resource", "message", err)
				//log.Errorf("Error when processing %v (for %v): %v.", path, rsync, err)
				log.Error(err)
				sentry.WithScope(func(scope *sentry.Scope) {
					if errC, ok := err.(interface{ SetSentryScope(*sentry.Scope) }); ok {
						errC.SetSentryScope(scope)
					}
					scope.SetTag("TrustAnchor", tal.Path)
					sentry.CaptureException(err)
				})
			}

			//log.Warn("Closed errors")
		}(sm, tal)
		manager[i].AddInitial([]*pki.PKIFile{tal})
		s.CountExplore = manager[i].Explore(!s.UseManifest, false)

		// Insertion of SIAs in db to allow rsync to update the repos
		var count int
		for _, obj := range manager[i].Validator.TALs {
			tal := obj.Resource.(*librpki.RPKITAL)
			//s.RsyncFetch[tal.GetURI()] = time.Now().UTC()
			if !obj.CertTALValid {
				s.TalsFetch[obj.File.Path] = tal
			}
			count++
		}
		for _, obj := range manager[i].Validator.ValidObjects {
			if obj.Type == pki.TYPE_CER {
				cer := obj.Resource.(*librpki.RPKICertificate)
				var RsyncGN string
				var RRDPGN string
				var hasRRDP bool
				for _, sia := range cer.SubjectInformationAccess {
					gn := string(sia.GeneralName)
					if sia.AccessMethod.Equal(CertRepository) {
						RsyncGN = gn
					} else if sia.AccessMethod.Equal(CertRRDP) {
						hasRRDP = true
						RRDPGN = gn
					}
				}
				gnExtracted, gnExtractedDomain, err := syncpki.ExtractRsyncDomainModule(RsyncGN)
				if err != nil {
					log.Errorf("Could not add cert rsync %s due to %v", RsyncGN, err)
					continue
				}

				if hasRRDP {
					prev, ok := s.RRDPFetchDomain[RRDPGN]
					if ok && prev != gnExtractedDomain {
						log.Errorf("rrdp %s tries to override %s with %s", RRDPGN, prev, gnExtractedDomain)
						continue
					}
					s.RRDPFetchDomain[RRDPGN] = gnExtractedDomain
					s.RRDPFetch[RRDPGN] = gnExtracted
				}
				s.RsyncFetch[gnExtracted] = RRDPGN
				s.CurrentRepos[gnExtracted] = time.Now().UTC()
				count++

				// map the rrdp and rsync by TAL for info page
				iaId, ok := iatmp[gnExtracted]
				if !ok {
					iaIdTmp := SIA{
						gnExtracted,
						RRDPGN,
					}
					ia[i] = append(ia[i], iaIdTmp)
					iaId = &(ia[i][len(ia[i])-1])
					iatmp[gnExtracted] = iaId
				}
				iaId.Rsync = gnExtracted
				iaId.RRDP = RRDPGN

			}
		}
		sm.Close()
		tSpan.LogKV("count-valid", count, "count-total", s.CountExplore)
		tSpan.Finish()
	}

	s.InfoAuthoritiesLock.Lock()
	s.InfoAuthorities = ia
	s.InfoAuthoritiesLock.Unlock()

	// Generating ROAs list
	roalist := &prefixfile.ROAList{
		Data: make([]prefixfile.ROAJson, 0),
	}
	var counts int
	s.ROAsTALsCount = make([]ROAsTAL, 0)
	for i, tal := range s.Tals {
		eSpan := tracer.StartSpan(
			"extract",
			opentracing.ChildOf(span.Context()),
		)
		eSpan.SetTag("tal", tal.Path)
		talname := tal.Path
		if len(s.TalNames) == len(s.Tals) {
			talname = s.TalNames[i]
		}

		var counttal int
		for _, obj := range manager[i].Validator.ValidROA {
			roa := obj.Resource.(*librpki.RPKIROA)

			for _, entry := range roa.Valids {
				oroa := prefixfile.ROAJson{
					ASN:    fmt.Sprintf("AS%v", roa.ASN),
					Prefix: entry.IPNet.String(),
					Length: uint8(entry.MaxLength),
					TA:     talname,
				}
				roalist.Data = append(roalist.Data, oroa)
				counts++
				counttal++
			}
		}
		eSpan.Finish()

		s.ROAsTALsCount = append(s.ROAsTALsCount, ROAsTAL{TA: talname, Count: counttal})
		MetricROAsCount.With(
			prometheus.Labels{
				"ta": talname,
			}).Set(float64(counttal))
	}
	curTime := time.Now().UTC()
	s.LastComputed = curTime
	validTime := curTime.Add(*ValidityDuration)
	roalist.Metadata = prefixfile.MetaData{
		Counts:    counts,
		Generated: int(curTime.Unix()),
		Valid:     int(validTime.Unix()),
	}

	roalist.Data = FilterDuplicates(roalist.Data)
	if s.Sign {
		sSpan := tracer.StartSpan(
			"sign",
			opentracing.ChildOf(span.Context()),
		)

		signdate, sign, err := roalist.Sign(s.Key)
		if err != nil {
			log.Error(err)
			sentry.CaptureException(err)
		}
		roalist.Metadata.Signature = sign
		roalist.Metadata.SignatureDate = signdate

		sSpan.Finish()
	}

	s.ROAListLock.Lock()
	s.ROAList = roalist
	s.ROAListLock.Unlock()
}

func (s *OctoRPKI) ServeROAs(w http.ResponseWriter, r *http.Request) {
	if s.Stable || !s.WaitStable || s.HasPreviousStable {

		upTo := s.LastComputed.Add(*ValidityDuration)
		maxAge := int(upTo.Sub(time.Now()).Seconds())

		w.Header().Set("Content-Type", "application/json")

		if maxAge > 0 && *CacheHeader {
			w.Header().Set("Cache-Control", fmt.Sprintf("max-age=%v", maxAge))
		}

		s.ROAListLock.RLock()
		tmp := s.ROAList
		s.ROAListLock.RUnlock()

		etag := sha256.New()
		etag.Write([]byte(fmt.Sprintf("%v/%v", tmp.Metadata.Generated, tmp.Metadata.Counts)))
		etagSum := etag.Sum(nil)
		etagSumHex := hex.EncodeToString(etagSum)

		if match := r.Header.Get("If-None-Match"); match != "" {
			if match == etagSumHex {
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}

		w.Header().Set("Etag", etagSumHex)
		enc := json.NewEncoder(w)
		enc.Encode(tmp)
	} else {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("File not ready yet"))
	}
}

func (s *OctoRPKI) ServeHealth(w http.ResponseWriter, r *http.Request) {
	if s.Stable || s.HasPreviousStable {
		w.WriteHeader(http.StatusOK)
		return
	}
	w.WriteHeader(http.StatusServiceUnavailable)
	w.Write([]byte("Not ready yet"))
}

type SIA struct {
	Rsync string `json:"rsync"`
	RRDP  string `json:"rrdp,omitempty"`
}

type ROAsTAL struct {
	TA    string `json:"ta,omitempty"`
	Count int    `json:"count,omitempty"`
}

type InfoAuthorities struct {
	TA  string `json:"name"`
	Sia []SIA  `json:"sia"`
}

type InfoResult struct {
	Stable             bool              `json:"stable"`
	TAs                []InfoAuthorities `json:"tas"`
	Iteration          int               `json:"iteration"`
	LastValidation     int               `json:"validation-last"`
	ValidationDuration float64           `json:"validation-duration"`
	ROAsTALs           []ROAsTAL         `json:"roas-tal-count"`
	ROACount           int               `json:"roas-count"`
}

func (s *OctoRPKI) ServeInfo(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	s.InfoAuthoritiesLock.RLock()
	ia := s.InfoAuthorities
	s.InfoAuthoritiesLock.RUnlock()

	ias := make([]InfoAuthorities, 0)
	for i, tal := range s.Tals {

		if len(ia) <= i {
			break
		}
		if ia[i] == nil {
			continue
		}

		talname := tal.Path
		if len(s.TalNames) == len(s.Tals) {
			talname = s.TalNames[i]
		}

		ias = append(ias, InfoAuthorities{
			TA:  talname,
			Sia: ia[i],
		})
	}

	ir := InfoResult{
		TAs:                ias,
		ROACount:           len(s.ROAList.Data),
		ROAsTALs:           s.ROAsTALsCount,
		Stable:             s.Stable,
		LastValidation:     int(s.LastComputed.UnixNano() / 1000000),
		ValidationDuration: s.ValidationDuration.Seconds(),
		Iteration:          s.Iteration,
	}
	enc := json.NewEncoder(w)
	enc.Encode(ir)
}

func (s *OctoRPKI) Serve(addr string, path string, metricsPath string, infoPath string, healthPath string, corsOrigin string, corsCreds bool) {
	// Note(Erica): fix https://github.com/cloudflare/cfrpki/issues/8
	fullPath := path
	if len(path) > 0 && string(path[0]) != "/" {
		fullPath = "/" + path
	}
	log.Infof("Serving HTTP on %v%v", addr, fullPath)

	r := http.NewServeMux()

	r.HandleFunc(fullPath, s.ServeROAs)
	r.HandleFunc(infoPath, s.ServeInfo)
	r.HandleFunc(healthPath, s.ServeHealth)
	r.Handle(metricsPath, promhttp.Handler())

	if *Pprof {
		r.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		r.HandleFunc("/debug/pprof/profile", pprof.Profile)
		r.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		r.HandleFunc("/debug/pprof/trace", pprof.Trace)
		r.HandleFunc("/debug/pprof/", pprof.Index)
	}

	corsReq := cors.New(cors.Options{
		AllowedOrigins:   strings.Split(corsOrigin, ","),
		AllowedMethods:   []string{"GET", "POST", "OPTIONS"},
		AllowCredentials: corsCreds,
	}).Handler(r)

	log.Fatal(http.ListenAndServe(addr, corsReq))
}

func init() {
	if !*AllowRoot && runningAsRoot() {
		panic("Running as root is not allowed by default")
	}

	prometheus.MustRegister(MetricSIACounts)
	prometheus.MustRegister(MetricRsyncErrors)
	prometheus.MustRegister(MetricRRDPErrors)
	prometheus.MustRegister(MetricRRDPSerial)
	prometheus.MustRegister(MetricROAsCount)
	prometheus.MustRegister(MetricState)
	prometheus.MustRegister(MetricLastStableValidation)
	prometheus.MustRegister(MetricLastValidation)
	prometheus.MustRegister(MetricOperationTime)
	prometheus.MustRegister(MetricLastFetch)
}

func runningAsRoot() bool {
	return os.Geteuid() == 0 || os.Getegid() == 0
}

func main() {
	runtime.GOMAXPROCS(runtime.NumCPU())

	flag.Parse()
	if *Version {
		fmt.Println(AppVersion)
		os.Exit(0)
	}

	lvl, _ := log.ParseLevel(*LogLevel)
	log.SetLevel(lvl)

	sentryDsn := *SentryDSN
	if sentryDsn == "" {
		sentryDsn = os.Getenv("SENTRY_DSN")
	}
	if sentryDsn != "" {
		err := sentry.Init(sentry.ClientOptions{
			Dsn: sentryDsn,
		})
		if err != nil {
			log.Fatalf("failed initializing sentry: %s", err)
		}
		defer sentry.Flush(2 * time.Second)
	}

	log.Info("Validator started")

	if *Tracer {
		cfg, err := jcfg.FromEnv()
		if err != nil {
			log.Fatal(err)
		}
		tracer, closer, err := cfg.NewTracer()
		if err != nil {
			log.Fatal(err)
		}
		defer closer.Close()
		opentracing.SetGlobalTracer(tracer)
	}

	mainRefresh, _ := time.ParseDuration(*Refresh)

	rootTALs := strings.Split(*RootTAL, ",")
	talNames := strings.Split(*TALNames, ",")
	tals := make([]*pki.PKIFile, 0)
	for _, tal := range rootTALs {
		tals = append(tals, &pki.PKIFile{
			Path: tal,
			Type: pki.TYPE_TAL,
		})
	}

	err := os.MkdirAll(*Basepath, os.ModePerm)
	if err != nil {
		log.Fatal(err)
	}

	s := &OctoRPKI{
		Basepath:    *Basepath,
		Tals:        tals,
		TalsFetch:   make(map[string]*librpki.RPKITAL),
		TalNames:    talNames,
		UseManifest: *UseManifest,

		WaitStable: *WaitStable,
		Sign:       *Sign,

		Mode:         *Mode,
		RRDPFailover: *RRDPFailover,

		RRDPInfo: make(map[string]RRDPInfo),

		PrevRepos:    make(map[string]time.Time),
		CurrentRepos: make(map[string]time.Time),

		RsyncFetch:      make(map[string]string),
		RRDPFetch:       make(map[string]string),
		RRDPFetchDomain: make(map[string]string),

		Fetcher: syncpki.NewLocalFetch(
			map[string]string{
				"rsync://": *Basepath,
			},
			log.StandardLogger()),
		HTTPFetcher: &syncpki.HTTPFetcher{
			UserAgent: *UserAgent,
			Client: &http.Client{
				// GHSA-8cvr-4rrf-f244: Prevent infinite open connections
				Timeout: time.Second * 60,
			},
		},
		ROAList: &prefixfile.ROAList{
			Data: make([]prefixfile.ROAJson, 0),
		},
		ROAListLock: &sync.RWMutex{},

		RsyncStats:    make(map[string]Stats),
		RRDPStats:     make(map[string]*Stats),
		ROAsTALsCount: make([]ROAsTAL, 0),

		InfoAuthorities:     make([][]SIA, 0),
		InfoAuthoritiesLock: &sync.RWMutex{},

		StrictHash:      *StrictHash,
		StrictManifests: *StrictManifests,
		StrictCms:       *StrictCms,
	}

	if *Sign {
		keyFile, err := os.Open(*SignKey)
		if err != nil {
			log.Fatal(err)
		}
		keyBytes, err := ioutil.ReadAll(keyFile)
		if err != nil {
			log.Fatal(err)
		}
		keyFile.Close()
		keyDec, err := ReadKey(keyBytes, true)
		if err != nil {
			log.Fatal(err)
		}
		s.Key = keyDec
	}

	if *Mode == "server" {
		go s.Serve(*Addr, *Output, *MetricsPath, *InfoPath, *HealthPath, *CorsOrigins, *CorsCreds)
	} else if *Mode != "oneoff" {
		log.Fatalf("Mode %v is not specified. Choose either server or oneoff", *Mode)
	}
	tracer := opentracing.GlobalTracer()

	var spanActive bool
	var pSpan opentracing.Span
	var iterationsUntilStable int
	for {
		if !spanActive {
			pSpan = tracer.StartSpan("multoperation")
			spanActive = true
			iterationsUntilStable = 0
		}

		span := tracer.StartSpan("operation", opentracing.ChildOf(pSpan.Context()))

		s.Iteration++
		iterationsUntilStable++
		// GHSA-g5gj-9ggf-9vmq: Prevent infinite repository traversal
		if iterationsUntilStable > *MaxIterations {
			log.Fatal("Max iterations has been reached. This number can be adjusted with -max.iterations")
		}
		span.SetTag("iteration", s.Iteration)

		if *RRDP {
			t1 := time.Now().UTC()
			// RRDP
			if *RRDPFile != "" {
				err = s.LoadRRDP(*RRDPFile)
				if err != nil {
					sentry.CaptureException(err)
				}
			}
			s.MainRRDP(span)
			if *RRDPFile != "" {
				s.SaveRRDP(*RRDPFile)
				if err != nil {
					sentry.CaptureException(err)
				}
			}

			t2 := time.Now().UTC()
			MetricOperationTime.With(
				prometheus.Labels{
					"type": "rrdp",
				}).
				Observe(float64(t2.Sub(t1).Seconds()))
		}

		t1 := time.Now().UTC()

		// HTTPs TAL
		s.MainTAL(span)
		s.TalsFetch = make(map[string]*librpki.RPKITAL) // clear decoded TAL for next iteration

		t2 := time.Now().UTC()
		MetricOperationTime.With(
			prometheus.Labels{
				"type": "tal",
			}).
			Observe(float64(t2.Sub(t1).Seconds()))

		t1 = time.Now().UTC()

		// Rsync
		s.MainRsync(span)

		t2 = time.Now().UTC()
		MetricOperationTime.With(
			prometheus.Labels{
				"type": "rsync",
			}).
			Observe(float64(t2.Sub(t1).Seconds()))

		s.ValidationMessages = make([]string, 0)
		t1 = time.Now().UTC()

		// Validation
		s.MainValidation(span)

		t2 = time.Now().UTC()
		s.ValidationDuration = t2.Sub(t1)
		MetricOperationTime.With(
			prometheus.Labels{
				"type": "validation",
			}).
			Observe(float64(s.ValidationDuration.Seconds()))
		MetricLastValidation.Set(float64(s.LastComputed.UnixNano() / 1000000000))

		t1 = time.Now().UTC()

		// Reduce
		s.Stable = !s.MainReduce() && s.Iteration > 1
		s.HasPreviousStable = s.Stable

		t2 = time.Now().UTC()
		MetricOperationTime.With(
			prometheus.Labels{
				"type": "reduce",
			}).
			Observe(float64(t2.Sub(t1).Seconds()))

		if *Mode == "oneoff" && (s.Stable || !*WaitStable) {
			if *Output == "" {
				enc := json.NewEncoder(os.Stdout)
				enc.Encode(s.ROAList)
			} else {
				f, err := os.Create(*Output)
				if err != nil {
					log.Fatal(err)
				}
				enc := json.NewEncoder(f)
				enc.Encode(s.ROAList)
				f.Close()
			}

		}

		span.SetTag("stable", s.Stable)
		span.Finish()

		if *Mode == "oneoff" && s.Stable {
			log.Info("Stable, terminating")
			break
		}

		if s.Stable {
			MetricLastStableValidation.Set(float64(s.LastComputed.UnixNano() / 1000000000))
			MetricState.Set(float64(1))

			pSpan.SetTag("iterations", iterationsUntilStable)
			pSpan.Finish()
			spanActive = false

			log.Infof("Stable state. Revalidating in %v", mainRefresh)
			<-time.After(mainRefresh)
			s.Stable = false
		} else {
			MetricState.Set(float64(0))

			log.Info("Still exploring. Revalidating now")
		}

	}
}
