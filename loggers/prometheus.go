package loggers

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/dmachard/go-dnscollector/dnsutils"
	"github.com/dmachard/go-logger"
	"github.com/dmachard/go-topmap"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type EpsCounters struct {
	Eps             uint64
	EpsMax          uint64
	TotalEvents     uint64
	TotalEventsPrev uint64
}

type Prometheus struct {
	done         chan bool
	done_api     chan bool
	httpserver   net.Listener
	channel      chan dnsutils.DnsMessage
	config       *dnsutils.Config
	logger       *logger.Logger
	promRegistry *prometheus.Registry
	version      string

	requesters map[string]map[string]int
	domains    map[string]map[string]int
	nxdomains  map[string]map[string]int

	topDomains    map[string]*topmap.TopMap
	topNxDomains  map[string]*topmap.TopMap
	topRequesters map[string]*topmap.TopMap

	requestersUniq map[string]int
	domainsUniq    map[string]int
	nxdomainsUniq  map[string]int

	streamsMap map[string]*EpsCounters

	gaugeBuildInfo     *prometheus.GaugeVec
	gaugeTopDomains    *prometheus.GaugeVec
	gaugeTopNxDomains  *prometheus.GaugeVec
	gaugeTopRequesters *prometheus.GaugeVec

	gaugeEps    *prometheus.GaugeVec
	gaugeEpsMax *prometheus.GaugeVec

	counterPackets     *prometheus.CounterVec
	totalReceivedBytes *prometheus.CounterVec
	totalSentBytes     *prometheus.CounterVec

	counterDomains    *prometheus.CounterVec
	counterDomainsNx  *prometheus.CounterVec
	counterRequesters *prometheus.CounterVec

	counterDomainsUniq    *prometheus.CounterVec
	counterDomainsNxUniq  *prometheus.CounterVec
	counterRequestersUniq *prometheus.CounterVec

	histogramQueriesLength *prometheus.HistogramVec
	histogramRepliesLength *prometheus.HistogramVec
	histogramQnamesLength  *prometheus.HistogramVec
	histogramLatencies     *prometheus.HistogramVec

	name string
}

func NewPrometheus(config *dnsutils.Config, logger *logger.Logger, version string, name string) *Prometheus {
	logger.Info("[%s] logger to prometheus - enabled", name)
	o := &Prometheus{
		done:         make(chan bool),
		done_api:     make(chan bool),
		config:       config,
		channel:      make(chan dnsutils.DnsMessage, 512),
		logger:       logger,
		version:      version,
		promRegistry: prometheus.NewRegistry(),

		requesters: make(map[string]map[string]int),
		domains:    make(map[string]map[string]int),
		nxdomains:  make(map[string]map[string]int),

		topDomains:    make(map[string]*topmap.TopMap),
		topNxDomains:  make(map[string]*topmap.TopMap),
		topRequesters: make(map[string]*topmap.TopMap),

		requestersUniq: make(map[string]int),
		domainsUniq:    make(map[string]int),
		nxdomainsUniq:  make(map[string]int),

		streamsMap: make(map[string]*EpsCounters),

		name: name,
	}
	o.InitProm()

	// add build version in metrics
	o.gaugeBuildInfo.WithLabelValues(o.version).Set(1)

	return o
}

func (o *Prometheus) InitProm() {
	o.gaugeBuildInfo = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: fmt.Sprintf("%s_build_info", o.config.Loggers.Prometheus.PromPrefix),
			Help: "Build version",
		},
		[]string{"version"},
	)
	o.promRegistry.MustRegister(o.gaugeBuildInfo)

	o.gaugeTopDomains = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: fmt.Sprintf("%s_top_domains_total", o.config.Loggers.Prometheus.PromPrefix),
			Help: "Number of hit per domain topN, partitioned by qname",
		},
		[]string{"stream_id", "domain"},
	)
	o.promRegistry.MustRegister(o.gaugeTopDomains)

	o.gaugeTopNxDomains = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: fmt.Sprintf("%s_top_nxdomains_total", o.config.Loggers.Prometheus.PromPrefix),
			Help: "Number of hit per nx domain topN, partitioned by qname",
		},
		[]string{"stream_id", "domain"},
	)
	o.promRegistry.MustRegister(o.gaugeTopNxDomains)

	o.gaugeTopRequesters = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: fmt.Sprintf("%s_top_requesters_total", o.config.Loggers.Prometheus.PromPrefix),
			Help: "Number of hit per requester topN, partitioned by qname",
		},
		[]string{"stream_id", "domain"},
	)
	o.promRegistry.MustRegister(o.gaugeTopRequesters)

	o.gaugeEps = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: fmt.Sprintf("%s_eps", o.config.Loggers.Prometheus.PromPrefix),
			Help: "Number of events per second received, partitioned by qname",
		},
		[]string{"stream_id"},
	)
	o.promRegistry.MustRegister(o.gaugeEps)

	o.gaugeEpsMax = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: fmt.Sprintf("%s_eps_max", o.config.Loggers.Prometheus.PromPrefix),
			Help: "Max number of events per second observed, partitioned by qname",
		},
		[]string{"stream_id"},
	)
	o.promRegistry.MustRegister(o.gaugeEpsMax)

	o.counterPackets = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: fmt.Sprintf("%s_packets_count", o.config.Loggers.Prometheus.PromPrefix),
			Help: "Counter of packets",
		},
		[]string{
			"stream_id",
			"net_family",
			"net_transport",
			"op_name",
			"op_code",
			"return_code",
			"query_type",
			"flag_qr",
			"flag_tc",
			"flag_aa",
			"flag_ra",
			"flag_ad",
			"pkt_err"},
	)
	o.promRegistry.MustRegister(o.counterPackets)

	o.histogramQueriesLength = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    fmt.Sprintf("%s_queries_size_bytes", o.config.Loggers.Prometheus.PromPrefix),
			Help:    "Size of the queries in bytes.",
			Buckets: []float64{50, 100, 250, 500},
		},
		[]string{"stream_id"},
	)
	o.promRegistry.MustRegister(o.histogramQueriesLength)

	o.histogramRepliesLength = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    fmt.Sprintf("%s_replies_size_bytes", o.config.Loggers.Prometheus.PromPrefix),
			Help:    "Size of the replies in bytes.",
			Buckets: []float64{50, 100, 250, 500},
		},
		[]string{"stream_id"},
	)
	o.promRegistry.MustRegister(o.histogramRepliesLength)

	o.histogramQnamesLength = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    fmt.Sprintf("%s_qnames_size_bytes", o.config.Loggers.Prometheus.PromPrefix),
			Help:    "Size of the qname in bytes.",
			Buckets: []float64{10, 20, 40, 60, 100},
		},
		[]string{"stream_id"},
	)
	o.promRegistry.MustRegister(o.histogramQnamesLength)

	o.histogramLatencies = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    fmt.Sprintf("%s_latencies", o.config.Loggers.Prometheus.PromPrefix),
			Help:    "Latency between query and reply",
			Buckets: []float64{0.001, 0.010, 0.050, 0.100, 0.5, 1.0},
		},
		[]string{"stream_id"},
	)
	o.promRegistry.MustRegister(o.histogramLatencies)

	o.totalReceivedBytes = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: fmt.Sprintf("%s_sent_bytes_total", o.config.Loggers.Prometheus.PromPrefix),
			Help: "The total bytes sent",
		},
		[]string{"stream_id"},
	)
	o.promRegistry.MustRegister(o.totalReceivedBytes)

	o.totalSentBytes = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: fmt.Sprintf("%s_received_bytes_total", o.config.Loggers.Prometheus.PromPrefix),
			Help: "The total bytes received",
		},
		[]string{"stream_id"},
	)
	o.promRegistry.MustRegister(o.totalSentBytes)

	o.counterDomains = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: fmt.Sprintf("%s_domains_count", o.config.Loggers.Prometheus.PromPrefix),
			Help: "The total number of domains per stream identity",
		},
		[]string{"stream_id"},
	)
	o.promRegistry.MustRegister(o.counterDomains)

	o.counterDomainsNx = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: fmt.Sprintf("%s_domains_nx_count", o.config.Loggers.Prometheus.PromPrefix),
			Help: "The total number of unknown domains per stream identity",
		},
		[]string{"stream_id"},
	)
	o.promRegistry.MustRegister(o.counterDomainsNx)

	o.counterRequesters = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: fmt.Sprintf("%s_requesters_count", o.config.Loggers.Prometheus.PromPrefix),
			Help: "The total number of DNS clients per stream identity",
		},
		[]string{"stream_id"},
	)
	o.promRegistry.MustRegister(o.counterRequesters)

	o.counterDomainsUniq = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: fmt.Sprintf("%s_domains_count_uniq", o.config.Loggers.Prometheus.PromPrefix),
			Help: "The total number of uniq domains per stream identity",
		},
		[]string{},
	)
	o.promRegistry.MustRegister(o.counterDomainsUniq)

	o.counterDomainsNxUniq = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: fmt.Sprintf("%s_domains_nx_count_uniq", o.config.Loggers.Prometheus.PromPrefix),
			Help: "The total number of uniq unknown domains per stream identity",
		},
		[]string{},
	)
	o.promRegistry.MustRegister(o.counterDomainsNxUniq)

	o.counterRequestersUniq = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: fmt.Sprintf("%s_requesters_count_uniq", o.config.Loggers.Prometheus.PromPrefix),
			Help: "The total number of uniq DNS clients per stream identity",
		},
		[]string{},
	)
	o.promRegistry.MustRegister(o.counterRequestersUniq)
}

func (o *Prometheus) ReadConfig() {
}

func (o *Prometheus) LogInfo(msg string, v ...interface{}) {
	o.logger.Info("["+o.name+"] prometheus - "+msg, v...)
}

func (o *Prometheus) LogError(msg string, v ...interface{}) {
	o.logger.Error("["+o.name+"] prometheus - "+msg, v...)
}

func (o *Prometheus) Channel() chan dnsutils.DnsMessage {
	return o.channel
}

func (o *Prometheus) Stop() {
	o.LogInfo("stopping...")

	// stopping http server
	o.httpserver.Close()

	// close output channel
	o.LogInfo("closing channel")
	close(o.channel)

	// read done channel and block until run is terminated
	<-o.done
	close(o.done)

	// block and wait until http api is terminated
	<-o.done_api
	close(o.done_api)

	o.LogInfo(" stopped")
}

/*func (o *Prometheus) BasicAuth(w http.ResponseWriter, r *http.Request) bool {
	login, password, authOK := r.BasicAuth()
	if !authOK {
		return false
	}

	return (login == o.config.Loggers.Prometheus.BasicAuthLogin) && (password == o.config.Loggers.Prometheus.BasicAuthPwd)
}*/

func (o *Prometheus) Record(dm dnsutils.DnsMessage) {
	// record stream identity
	if _, exists := o.streamsMap[dm.DnsTap.Identity]; !exists {
		o.streamsMap[dm.DnsTap.Identity] = new(EpsCounters)
		o.streamsMap[dm.DnsTap.Identity].TotalEvents = 1
	} else {
		o.streamsMap[dm.DnsTap.Identity].TotalEvents += 1
	}

	// count number of logs according to the stream name
	//o.counterPackets.WithLabelValues(dm.DnsTap.Identity).Inc()
	o.counterPackets.WithLabelValues(
		dm.DnsTap.Identity,
		dm.NetworkInfo.Family,
		dm.NetworkInfo.Protocol,
		dm.DnsTap.Operation,
		strconv.Itoa(dm.DNS.Opcode),
		dm.DNS.Rcode,
		dm.DNS.Qtype,
		dm.DNS.Type,
		strconv.FormatBool(dm.DNS.Flags.TC),
		strconv.FormatBool(dm.DNS.Flags.AA),
		strconv.FormatBool(dm.DNS.Flags.RA),
		strconv.FormatBool(dm.DNS.Flags.AD),
		strconv.FormatBool(dm.DNS.MalformedPacket),
	).Inc()

	// count the number of queries and replies
	// count the total bytes for queries and replies
	// and then make a histogram for queries and replies packet length observed
	if dm.DNS.Type == dnsutils.DnsQuery {
		o.totalReceivedBytes.WithLabelValues(dm.DnsTap.Identity).Add(float64(dm.DNS.Length))
		o.histogramQueriesLength.WithLabelValues(dm.DnsTap.Identity).Observe(float64(dm.DNS.Length))
	} else {
		o.totalSentBytes.WithLabelValues(dm.DnsTap.Identity).Add(float64(dm.DNS.Length))
		o.histogramRepliesLength.WithLabelValues(dm.DnsTap.Identity).Observe(float64(dm.DNS.Length))
	}

	// make histogram for qname length observed
	o.histogramQnamesLength.WithLabelValues(dm.DnsTap.Identity).Observe(float64(len(dm.DNS.Qname)))

	// make histogram for latencies observed
	if dm.DnsTap.Latency > 0.0 {
		o.histogramLatencies.WithLabelValues(dm.DnsTap.Identity).Observe(dm.DnsTap.Latency)
	}

	/* count all domains name and top domains */
	if _, exists := o.domainsUniq[dm.DNS.Qname]; !exists {
		o.domainsUniq[dm.DNS.Qname] = 1
		o.counterDomainsUniq.WithLabelValues().Inc()
	} else {
		o.domainsUniq[dm.DNS.Qname] += 1
	}

	if _, exists := o.domains[dm.DnsTap.Identity]; !exists {
		o.domains[dm.DnsTap.Identity] = make(map[string]int)
	}

	if _, exists := o.domains[dm.DnsTap.Identity][dm.DNS.Qname]; !exists {
		o.domains[dm.DnsTap.Identity][dm.DNS.Qname] = 1
		o.counterDomains.WithLabelValues(dm.DnsTap.Identity).Inc()
	} else {
		o.domains[dm.DnsTap.Identity][dm.DNS.Qname] += 1
	}

	if _, ok := o.topDomains[dm.DnsTap.Identity]; !ok {
		o.topDomains[dm.DnsTap.Identity] = topmap.NewTopMap(o.config.Loggers.Prometheus.TopN)
	}
	o.topDomains[dm.DnsTap.Identity].Record(dm.DNS.Qname, o.domains[dm.DnsTap.Identity][dm.DNS.Qname])

	o.gaugeTopDomains.Reset()
	for _, r := range o.topDomains[dm.DnsTap.Identity].Get() {
		o.gaugeTopDomains.WithLabelValues(dm.DnsTap.Identity, r.Name).Set(float64(r.Hit))
	}

	/* record and count all nx domains name and topN*/
	if dm.DNS.Rcode == "NXDOMAIN" {
		if _, exists := o.nxdomainsUniq[dm.DNS.Qname]; !exists {
			o.nxdomainsUniq[dm.DNS.Qname] = 1
			o.counterDomainsNxUniq.WithLabelValues().Inc()
		} else {
			o.nxdomainsUniq[dm.DNS.Qname] += 1
		}

		if _, exists := o.nxdomains[dm.DnsTap.Identity]; !exists {
			o.nxdomains[dm.DnsTap.Identity] = make(map[string]int)
		}
		if _, exists := o.nxdomains[dm.DnsTap.Identity][dm.DNS.Qname]; !exists {
			o.nxdomains[dm.DnsTap.Identity][dm.DNS.Qname] = 1
			o.counterDomainsNx.WithLabelValues(dm.DnsTap.Identity).Inc()
		} else {
			o.nxdomains[dm.DnsTap.Identity][dm.DNS.Qname] += 1
		}

		if _, ok := o.topNxDomains[dm.DnsTap.Identity]; !ok {
			o.topNxDomains[dm.DnsTap.Identity] = topmap.NewTopMap(o.config.Loggers.Prometheus.TopN)
		}
		o.topNxDomains[dm.DnsTap.Identity].Record(dm.DNS.Qname, o.domains[dm.DnsTap.Identity][dm.DNS.Qname])

		o.gaugeTopNxDomains.Reset()
		for _, r := range o.topNxDomains[dm.DnsTap.Identity].Get() {
			o.gaugeTopNxDomains.WithLabelValues(dm.DnsTap.Identity, r.Name).Set(float64(r.Hit))
		}
	}

	// record all clients and topN
	if _, ok := o.requestersUniq[dm.NetworkInfo.QueryIp]; !ok {
		o.requestersUniq[dm.NetworkInfo.QueryIp] = 1
		o.counterRequestersUniq.WithLabelValues().Inc()
	} else {
		o.requestersUniq[dm.NetworkInfo.QueryIp] += 1
	}

	if _, exists := o.requesters[dm.DnsTap.Identity]; !exists {
		o.requesters[dm.DnsTap.Identity] = make(map[string]int)
	}
	if _, ok := o.requesters[dm.DnsTap.Identity][dm.NetworkInfo.QueryIp]; !ok {
		o.requesters[dm.DnsTap.Identity][dm.NetworkInfo.QueryIp] = 1
		o.counterRequesters.WithLabelValues(dm.DnsTap.Identity).Inc()
	} else {
		o.requesters[dm.DnsTap.Identity][dm.NetworkInfo.QueryIp] += 1
	}

	if _, ok := o.topRequesters[dm.DnsTap.Identity]; !ok {
		o.topRequesters[dm.DnsTap.Identity] = topmap.NewTopMap(o.config.Loggers.Prometheus.TopN)
	}
	o.topRequesters[dm.DnsTap.Identity].Record(dm.DNS.Qname, o.domains[dm.DnsTap.Identity][dm.DNS.Qname])

	o.gaugeTopRequesters.Reset()
	for _, r := range o.topRequesters[dm.DnsTap.Identity].Get() {
		o.gaugeTopRequesters.WithLabelValues(dm.DnsTap.Identity, r.Name).Set(float64(r.Hit))
	}
}

func (o *Prometheus) ComputeEps() {
	// for each stream compute the number of events per second
	for stream := range o.streamsMap {
		// compute number of events per second
		if o.streamsMap[stream].TotalEvents > 0 && o.streamsMap[stream].TotalEventsPrev > 0 {
			o.streamsMap[stream].Eps = o.streamsMap[stream].TotalEvents - o.streamsMap[stream].TotalEventsPrev
		}
		o.streamsMap[stream].TotalEventsPrev = o.streamsMap[stream].TotalEvents

		// kept the max number of events per second
		if o.streamsMap[stream].Eps > o.streamsMap[stream].EpsMax {
			o.streamsMap[stream].EpsMax = o.streamsMap[stream].Eps
		}

		o.gaugeEps.WithLabelValues(stream).Set(float64(o.streamsMap[stream].Eps))
		o.gaugeEpsMax.WithLabelValues(stream).Set(float64(o.streamsMap[stream].EpsMax))
	}
}

func (s *Prometheus) ListenAndServe() {
	s.LogInfo("starting prometheus metrics...")

	mux := http.NewServeMux()

	mux.Handle("/metrics", promhttp.HandlerFor(s.promRegistry, promhttp.HandlerOpts{}))

	var err error
	var listener net.Listener
	addrlisten := s.config.Loggers.Prometheus.ListenIP + ":" + strconv.Itoa(s.config.Loggers.Prometheus.ListenPort)
	// listening with tls enabled ?
	if s.config.Loggers.Prometheus.TlsSupport {
		s.LogInfo("tls support enabled")
		var cer tls.Certificate
		cer, err = tls.LoadX509KeyPair(s.config.Loggers.Prometheus.CertFile, s.config.Loggers.Prometheus.KeyFile)
		if err != nil {
			s.logger.Fatal("loading certificate failed:", err)
		}

		config := &tls.Config{
			Certificates: []tls.Certificate{cer},
		}

		if s.config.Loggers.Prometheus.TlsMutual {

			// Create a CA certificate pool and add cert.pem to it
			var caCert []byte
			caCert, err = ioutil.ReadFile(s.config.Loggers.Prometheus.CertFile)
			if err != nil {
				s.logger.Fatal(err)
			}
			caCertPool := x509.NewCertPool()
			caCertPool.AppendCertsFromPEM(caCert)

			config.ClientCAs = caCertPool
			config.ClientAuth = tls.RequireAndVerifyClientCert
		}

		listener, err = tls.Listen("tcp", addrlisten, config)

	} else {
		// basic listening
		listener, err = net.Listen("tcp", addrlisten)
	}

	// something wrong ?
	if err != nil {
		s.logger.Fatal("listening failed:", err)
	}

	s.httpserver = listener
	s.LogInfo("is listening on %s", listener.Addr())

	srv := &http.Server{Handler: mux, ErrorLog: s.logger.ErrorLogger()}
	srv.Serve(s.httpserver)

	s.LogInfo("terminated")
	s.done_api <- true
}

func (s *Prometheus) Run() {
	s.LogInfo("running in background...")

	// start http server
	go s.ListenAndServe()

	// init timer to compute qps
	t1_interval := 1 * time.Second
	t1 := time.NewTimer(t1_interval)

LOOP:
	for {
		select {
		case dm, opened := <-s.channel:
			if !opened {
				s.LogInfo("channel closed")
				break LOOP
			}
			// record the dnstap message
			s.Record(dm)

		case <-t1.C:
			// compute eps each second
			s.ComputeEps()

			// reset the timer
			t1.Reset(t1_interval)
		}

	}
	s.LogInfo("run terminated")

	// the job is done
	s.done <- true
}
