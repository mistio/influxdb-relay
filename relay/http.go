package relay

import (
	"bytes"
	"compress/gzip"
	"crypto/tls"
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	log "github.com/golang/glog"

	"github.com/influxdata/influxdb/models"
	"github.com/influxdata/telegraf/plugins/outputs/graphite"

	"github.com/robfig/cron"
)

// HTTP is a relay for HTTP influxdb writes
type HTTP struct {
	addr   string
	name   string
	schema string

	cert string
	rp   string

	closing int64
	l       net.Listener

	enableMetering bool
	ampqURL        string

	dropUnauthorized bool

	cronJob      *cron.Cron
	cronSchedule string

	maxDatapointsPerRequest   int
	splitRequestPerDatapoints int
	itsAllGoodMan             bool

	backends []*httpBackend
}

const (
	DefaultHTTPTimeout      = 10 * time.Second
	DefaultMaxDelayInterval = 10 * time.Second
	DefaultBatchSizeKB      = 512

	KB = 1024
	MB = 1024 * KB
)

func NewHTTP(cfg HTTPConfig) (Relay, error) {
	h := new(HTTP)

	h.addr = cfg.Addr
	h.name = cfg.Name

	h.cert = cfg.SSLCombinedPem
	h.rp = cfg.DefaultRetentionPolicy

	h.schema = "http"
	if h.cert != "" {
		h.schema = "https"
	}

	for i := range cfg.Outputs {
		backend, err := newHTTPBackend(&cfg.Outputs[i])
		if err != nil {
			return nil, err
		}

		log.Infof("New backend with type: %s\n", backend.backendType)
		h.backends = append(h.backends, backend)
	}

	h.enableMetering = cfg.EnableMetering
	h.ampqURL = cfg.AMQPUrl
	amqpURL = cfg.AMQPUrl

	if h.enableMetering && h.ampqURL == "" {
		h.enableMetering = false
		log.Warning("You have to set AMQPUrl in config for metering to work")
		log.Warning("Disabling metering for now")
	}

	h.dropUnauthorized = cfg.DropUnauthorized

	h.cronSchedule = cfg.CronSchedule

	if h.cronSchedule != "" {
		h.cronJob = cron.New()
	}

	h.maxDatapointsPerRequest = cfg.MaxDatapointsPerRequest
	if cfg.SplitRequestPerDatapoints == 0 {
		// Use maxint if we don't want to split
		h.splitRequestPerDatapoints = int(^uint(0) >> 1)
	} else {
		h.splitRequestPerDatapoints = cfg.SplitRequestPerDatapoints
	}
	h.itsAllGoodMan = cfg.ItsAllGoodMan

	return h, nil
}

func (h *HTTP) Name() string {
	if h.name == "" {
		return fmt.Sprintf("%s://%s", h.schema, h.addr)
	}
	return h.name
}

func (h *HTTP) Run() error {
	l, err := net.Listen("tcp", h.addr)

	if h.cronSchedule != "" {
		h.cronJob.AddFunc(h.cronSchedule, pushToAmqp)
		h.cronJob.Start()
	}

	if err != nil {
		return err
	}

	// support HTTPS
	if h.cert != "" {
		cert, err := tls.LoadX509KeyPair(h.cert, h.cert)
		if err != nil {
			return err
		}

		l = tls.NewListener(l, &tls.Config{
			Certificates: []tls.Certificate{cert},
		})
	}

	h.l = l

	log.Infof("Starting %s relay %q on %v", strings.ToUpper(h.schema), h.Name(), h.addr)

	err = http.Serve(l, h)
	if atomic.LoadInt64(&h.closing) != 0 {
		return nil
	}
	return err
}

func (h *HTTP) Stop() error {
	atomic.StoreInt64(&h.closing, 1)
	if h.cronSchedule != "" {
		h.cronJob.Stop()
	}
	return h.l.Close()
}

func (h *HTTP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	if r.URL.Path == "/ping" && (r.Method == "GET" || r.Method == "HEAD") {
		w.Header().Add("X-InfluxDB-Version", "relay")
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if r.URL.Path != "/write" {
		jsonError(w, http.StatusNotFound, "invalid write endpoint")
		log.Error("Invalid write endpoint")
		return
	}

	if r.Method != "POST" {
		w.Header().Set("Allow", "POST")
		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusNoContent)
		} else {
			jsonError(w, http.StatusMethodNotAllowed, "invalid write method")
			log.Error("Invalid write method")
		}
		return
	}

	queryParams := r.URL.Query()

	if queryParams.Get("rp") == "" && h.rp != "" {
		queryParams.Set("rp", h.rp)
	}

	var body = r.Body

	if r.Header.Get("Content-Encoding") == "gzip" {
		b, err := gzip.NewReader(r.Body)
		if err != nil {
			jsonError(w, http.StatusBadRequest, "unable to decode gzip body")
			log.Error("Unable to decode gzip body")
		}
		defer b.Close()
		body = b
	}

	bodyBuf := getBuf()
	_, err := bodyBuf.ReadFrom(body)
	if err != nil {
		if h.itsAllGoodMan {
			w.WriteHeader(204)
		} else {
			jsonError(w, http.StatusInternalServerError, "problem reading request body")
		}
		machineID := ""
		if r.Header["X-Gocky-Tag-Machine-Id"] != nil {
			machineID = r.Header["X-Gocky-Tag-Machine-Id"][0]
		}
		if log.V(5) {
			log.Errorf("Problem reading request body from machine: %s and body %v", machineID, bodyBuf)
		} else {
			log.Errorf("Problem reading request body from machine: %s", machineID)
		}
		putBuf(bodyBuf)
		return
	}

	precision := queryParams.Get("precision")
	points, err := models.ParsePointsWithPrecision(bodyBuf.Bytes(), start, precision)
	if err != nil {
		putBuf(bodyBuf)
		jsonError(w, http.StatusBadRequest, "unable to parse points")
		log.Error("Unable to parse points")
		return
	}

	graphiteBuf := getBuf()
	for _, p := range points {
		if _, err = graphiteBuf.WriteString(p.PrecisionString(precision)); err != nil {
			break
		}
		if err = graphiteBuf.WriteByte('\n'); err != nil {
			break
		}
	}

	if err != nil {
		putBuf(graphiteBuf)
		jsonError(w, http.StatusInternalServerError, "problem writing points")
		log.Error("Problem writing points")
		return
	}

	outBytes := [][]byte{}

	metricsMap := make(map[string]bool)

	totalDatapoints := parseRequest(h.splitRequestPerDatapoints, &outBytes, metricsMap, points)

	machineID := ""
	if r.Header["X-Gocky-Tag-Machine-Id"] != nil {
		machineID = r.Header["X-Gocky-Tag-Machine-Id"][0]
	} else {
		if h.dropUnauthorized {
			log.Error("Gocky Headers are missing. Dropping packages...")
			jsonError(w, http.StatusForbidden, "cannot find Gocky headers")
			return
		}
	}

	if h.maxDatapointsPerRequest > 0 && totalDatapoints > h.maxDatapointsPerRequest {
		log.Errorf("Payload too large for resource: %s, number of metrics: %d, number of datapoints: %d\n", machineID, len(metricsMap), totalDatapoints)
		w.WriteHeader(204)
		return
	}

	log.Infof("Request for resource: %s, number of metrics: %d, number of datapoints: %d\n", machineID, len(metricsMap), totalDatapoints)

	orgID := "Unauthorized"
	if r.Header["X-Gocky-Tag-Org-Id"] != nil {
		orgID = r.Header["X-Gocky-Tag-Org-Id"][0]
	}

	if h.enableMetering {
		mu.Lock()

		_, orgExists := metering[orgID]
		if !orgExists {
			metering[orgID] = make(map[string]int)
		}

		_, machExists := metering[orgID][machineID]
		if !machExists {
			metering[orgID][machineID] = len(points)
		} else {
			metering[orgID][machineID] += len(points)
		}

		mu.Unlock()
	}

	sourceType := "unix"

	if r.Header["X-Gocky-Tag-Source-Type"][0] == "windows" {
		sourceType = "windows"
	}

	// normalize query string
	query := queryParams.Encode()

	// check for authorization performed via the header
	authHeader := r.Header.Get("Authorization")

	influxdbBackends := 0

	for _, b := range h.backends {
		if b.backendType == "influxdb" {
			influxdbBackends++
		}
	}

	var wg sync.WaitGroup
	wg.Add(len(h.backends) - influxdbBackends + influxdbBackends*len(outBytes))

	var once sync.Once

	var responses = make(chan *responseData, len(h.backends)-influxdbBackends+influxdbBackends*len(outBytes))

	ignoreResponses := false

	if h.itsAllGoodMan {
		ignoreResponses = true
		w.WriteHeader(204)
	} else {
		for _, b := range h.backends {
			if b.backendType == "graphite" {
				ignoreResponses = true
				w.WriteHeader(204)
				break
			}
		}
	}

	for _, b := range h.backends {
		b := b
		if b.backendType == "influxdb" {
			// fail early if we're missing the database
			if queryParams.Get("db") == "" {
				jsonError(w, http.StatusBadRequest, "missing parameter: db")
				log.Error("Missing parameter: db")
				return
			}
			for i := range outBytes {
				outByte := outBytes[i]
				go func() {
					defer wg.Done()
					resp, err := pushToInfluxdb(b, outByte, query, authHeader, orgID)
					if err != nil {
						log.Errorf("Problem posting to relay %q backend %q: %v", h.Name(), b.name, err)
					} else if resp.StatusCode / 100 == 5 {
						log.Errorf("5xx response for relay %q backend %q: %v", h.Name(), b.name, resp.StatusCode)
					}
					if !ignoreResponses {
						resp.HandleResponse(h, w, b, responses, &once)
					}
				}()
			}
		} else if b.backendType == "graphite" {
			graphiteServers := make([]string, 1)
			graphiteServers[0] = b.location
			graphiteClient := &graphite.Graphite{
				Servers: graphiteServers,
				Prefix:  "bucky",
			}

			conErr := graphiteClient.Connect()
			if conErr != nil {
				jsonError(w, http.StatusInternalServerError, "unable to connect to graphite")
				log.Fatalf("Could not connect to graphite: %s", conErr)
			}

			newPoints, err := models.ParsePointsWithPrecision(graphiteBuf.Bytes(), start, precision)
			if err != nil {
				jsonError(w, http.StatusBadRequest, "unable to parse points")
				log.Error("Unable to parse points")
				return
			}
			go pushToGraphite(newPoints, graphiteClient, machineID, sourceType)
			wg.Done()
		} else {
			wg.Done()
			log.Errorf("Unknown backend type: %q posting to relay: %q with backend name: %q", b.backendType, h.Name(), b.name)
		}

	}

	go func() {
		wg.Wait()
		close(responses)
	}()

	var errResponse *responseData

	for resp := range responses {
		switch resp.StatusCode / 100 {
		case 2:
			return

		case 4:
			// user error
			return

		default:
			// hold on to one of the responses to return back to the client
			errResponse = resp
		}
	}
	if !ignoreResponses {
		// no successful writes
		if errResponse == nil {
			// failed to make any valid request...
			jsonError(w, http.StatusServiceUnavailable, "unable to write points")
			log.Error("Unable to write points")
			return
		}

		errResponse.Write(w)
	}
}

type responseData struct {
	ContentType     string
	ContentEncoding string
	StatusCode      int
	Body            []byte
}

func (rd *responseData) Write(w http.ResponseWriter) {
	if rd.ContentType != "" {
		w.Header().Set("Content-Type", rd.ContentType)
	}

	if rd.ContentEncoding != "" {
		w.Header().Set("Content-Encoding", rd.ContentEncoding)
	}

	w.Header().Set("Content-Length", strconv.Itoa(len(rd.Body)))
	w.WriteHeader(rd.StatusCode)
	w.Write(rd.Body)
}

func (rd *responseData) HandleResponse(h *HTTP, w http.ResponseWriter, b *httpBackend, responses chan *responseData, once *sync.Once) {

	onFirstSuccess := func() {
		w.WriteHeader(http.StatusNoContent)
	}

	onFirstUserError := func() {
		rd.Write(w)
	}

	switch rd.StatusCode / 100 {
	case 2:
		once.Do(onFirstSuccess)

	case 4:
		// user error
		once.Do(onFirstUserError)
	}
	responses <- rd
}

func jsonError(w http.ResponseWriter, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	data := fmt.Sprintf("{\"error\":%q}\n", message)
	w.Header().Set("Content-Length", fmt.Sprint(len(data)))
	w.WriteHeader(code)
	w.Write([]byte(data))
}

type poster interface {
	post([]byte, string, string, string) (*responseData, error)
}

type simplePoster struct {
	client   *http.Client
	location string
}

func newSimplePoster(location string, timeout time.Duration, skipTLSVerification bool) *simplePoster {
	// Configure custom transport for http.Client
	// Used for support skip-tls-verification option
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: skipTLSVerification,
		},
	}

	return &simplePoster{
		client: &http.Client{
			Timeout:   timeout,
			Transport: transport,
		},
		location: location,
	}
}

func (b *simplePoster) post(buf []byte, query string, auth string, org string) (*responseData, error) {
	req, err := http.NewRequest("POST", b.location, bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}

	req.URL.RawQuery = query
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("Content-Length", strconv.Itoa(len(buf)))
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	req.Header.Set("x-org-id", org)

	resp, err := b.client.Do(req)
	if err != nil {
		return nil, err
	}

	data, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if err = resp.Body.Close(); err != nil {
		return nil, err
	}

	return &responseData{
		ContentType:     resp.Header.Get("Conent-Type"),
		ContentEncoding: resp.Header.Get("Conent-Encoding"),
		StatusCode:      resp.StatusCode,
		Body:            data,
	}, nil
}

type httpBackend struct {
	poster
	name        string
	backendType string
	location    string
}

func newHTTPBackend(cfg *HTTPOutputConfig) (*httpBackend, error) {
	if cfg.Name == "" {
		cfg.Name = cfg.Location
	}

	timeout := DefaultHTTPTimeout
	if cfg.Timeout != "" {
		t, err := time.ParseDuration(cfg.Timeout)
		if err != nil {
			return nil, fmt.Errorf("error parsing HTTP timeout '%v'", err)
		}
		timeout = t
	}

	if cfg.BackendType == "influxdb" {
		var p poster = newSimplePoster(cfg.Location, timeout, cfg.SkipTLSVerification)

		// If configured, create a retryBuffer per backend.
		// This way we serialize retries against each backend.
		if cfg.BufferSizeMB > 0 {
			max := DefaultMaxDelayInterval
			if cfg.MaxDelayInterval != "" {
				m, err := time.ParseDuration(cfg.MaxDelayInterval)
				if err != nil {
					return nil, fmt.Errorf("error parsing max retry time %v", err)
				}
				max = m
			}

			batch := DefaultBatchSizeKB * KB
			if cfg.MaxBatchKB > 0 {
				batch = cfg.MaxBatchKB * KB
			}

			p = newRetryBuffer(cfg.BufferSizeMB*MB, batch, max, p)
		}

		return &httpBackend{
			poster:      p,
			name:        cfg.Name,
			backendType: cfg.BackendType,
			location:    "",
		}, nil
	}

	return &httpBackend{
		poster:      nil,
		name:        cfg.Name,
		backendType: cfg.BackendType,
		location:    cfg.Location,
	}, nil
}

var ErrBufferFull = errors.New("retry buffer full")

var bufPool = sync.Pool{New: func() interface{} { return new(bytes.Buffer) }}

func getBuf() *bytes.Buffer {
	if bb, ok := bufPool.Get().(*bytes.Buffer); ok {
		return bb
	}
	return new(bytes.Buffer)
}

func putBuf(b *bytes.Buffer) {
	b.Reset()
	bufPool.Put(b)
}

func pushToInfluxdb(b *httpBackend, buf []byte, query string, auth string, org string) (*responseData, error) {
	resp, err := b.post(buf, query, auth, org)
	// These retries are necessary because by default we use the simplePoster which has no retries
	for i := 0; i < 3; i++ {
		if err == nil {
			break
		}
		log.Error(err)
		log.Errorf("Retrying to send datapoints to influxdb backend: %s\n", b.location)
		time.Sleep(1000 * time.Millisecond)
		resp, err = b.post(buf, query, auth, org)
	}
	return resp, err
}

// Parses and counts influxdb points. Optionally splits them into multiple requests.
func parseRequest(splitRequestPerDatapoints int, outBytes *[][]byte, metricsMap map[string]bool, points models.Points) int {
	datapointsLeft := splitRequestPerDatapoints

	linesToSend := ""

	totalDatapoints := 0

	for _, p := range points {

		f := p.FieldIterator()
		measurementAndTags := string(p.Key())
		newLine := measurementAndTags + " "
		field := ""
		numOfFields := 0

		for f.Next() {
			switch f.Type() {
			case models.Float:
				v, _ := f.FloatValue()
				if utf8.ValidString(string(f.FieldKey())) {
					field = string(f.FieldKey()) + "=" + strconv.FormatFloat(v, 'E', -1, 64)
					metricsMap[measurementAndTags+string(f.FieldKey())] = true
				} else {
					continue
				}
			case models.Integer:
				v, _ := f.IntegerValue()
				if utf8.ValidString(string(f.FieldKey())) {
					field = string(f.FieldKey()) + "=" + strconv.FormatInt(v, 10)
					metricsMap[measurementAndTags+string(f.FieldKey())] = true
				} else {
					continue
				}
			default:
				continue
			}

			numOfFields++
			newLine += field + ","
		}

		if numOfFields == 0 {
			continue
		}

		newLine = strings.TrimSuffix(newLine, ",") + " " + strconv.FormatInt(p.UnixNano(), 10) + "\n"

		if datapointsLeft-numOfFields >= 0 {
			linesToSend += newLine
			datapointsLeft -= numOfFields
		} else {
			*outBytes = append(*outBytes, []byte(linesToSend))
			linesToSend = newLine
			datapointsLeft = splitRequestPerDatapoints - numOfFields
		}

		totalDatapoints += numOfFields
	}

	if len(linesToSend) > 0 {
		*outBytes = append(*outBytes, []byte(linesToSend))
	}

	return totalDatapoints
}
