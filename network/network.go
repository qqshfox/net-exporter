package network

import (
	"fmt"
	"net"
	"time"

	"github.com/giantswarm/exporterkit/histogramvec"
	"github.com/giantswarm/microerror"
	"github.com/giantswarm/micrologger"
	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	namespace = "network"

	bucketStart  = 0.001
	bucketFactor = 2
	numBuckets   = 15
)

// Config provides the necessary configuration for creating a Collector.
type Config struct {
	Dialer           *net.Dialer
	KubernetesClient kubernetes.Interface
	Logger           micrologger.Logger

	Namespace string
	Port      string
	Service   string
}

// Collector implements the Collector interface, exposing network latency information.
type Collector struct {
	dialer           *net.Dialer
	kubernetesClient kubernetes.Interface
	logger           micrologger.Logger

	namespace string
	port      string
	service   string

	latencyHistogramVec  *histogramvec.HistogramVec
	latencyHistogramDesc *prometheus.Desc

	errorCount     prometheus.Counter
	dialErrorCount *prometheus.CounterVec
}

// New creates a Collector, given a Config.
func New(config Config) (*Collector, error) {
	if config.Dialer == nil {
		return nil, microerror.Maskf(invalidConfigError, "%T.Dialer must not be empty", config)
	}
	if config.KubernetesClient == nil {
		return nil, microerror.Maskf(invalidConfigError, "%T.KubernetesClient must not be empty", config)
	}
	if config.Logger == nil {
		return nil, microerror.Maskf(invalidConfigError, "%T.Logger must not be empty", config)
	}

	if config.Namespace == "" {
		return nil, microerror.Maskf(invalidConfigError, "%T.Namespace must not be empty", config)
	}
	if config.Port == "" {
		return nil, microerror.Maskf(invalidConfigError, "%T.Port must not be empty", config)
	}
	if config.Service == "" {
		return nil, microerror.Maskf(invalidConfigError, "%T.Service must not be empty", config)
	}

	var err error
	var latencyHistogramVec *histogramvec.HistogramVec
	{
		c := histogramvec.Config{
			BucketLimits: prometheus.ExponentialBuckets(bucketStart, bucketFactor, numBuckets),
		}
		latencyHistogramVec, err = histogramvec.New(c)
		if err != nil {
			return nil, microerror.Mask(err)
		}
	}

	errorCount := prometheus.NewCounter(prometheus.CounterOpts{
		Name: prometheus.BuildFQName(namespace, "", "error_total"),
		Help: "Total number of internal errors.",
	})
	dialErrorCount := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: prometheus.BuildFQName(namespace, "", "dial_error_total"),
			Help: "Total number of errors dialing hosts.",
		},
		[]string{"host"},
	)

	prometheus.MustRegister(errorCount)
	prometheus.MustRegister(dialErrorCount)

	collector := &Collector{
		dialer:           config.Dialer,
		kubernetesClient: config.KubernetesClient,
		logger:           config.Logger,

		namespace: config.Namespace,
		port:      config.Port,
		service:   config.Service,

		latencyHistogramVec: latencyHistogramVec,
		latencyHistogramDesc: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "latency_seconds"),
			"Histogram of latency of network dials.",
			[]string{"host"},
			nil,
		),

		errorCount:     errorCount,
		dialErrorCount: dialErrorCount,
	}

	return collector, nil
}

// Describe implements the Describe method of the Collector interface.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.latencyHistogramDesc
}

// Collect implements the Collect method of the Collector interface.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	hosts := []string{}

	service, err := c.kubernetesClient.CoreV1().Services(c.namespace).Get(c.service, metav1.GetOptions{})
	if err != nil {
		c.logger.Log("level", "error", "message", "could not get service", "stack", microerror.Stack(err))
		c.errorCount.Inc()
		return
	}

	hosts = append(hosts, fmt.Sprintf("%v:%v", service.Spec.ClusterIP, c.port))

	endpoints, err := c.kubernetesClient.CoreV1().Endpoints(c.namespace).Get(c.service, metav1.GetOptions{})
	if err != nil {
		c.logger.Log("level", "error", "message", "could not get endpoints", "stack", microerror.Stack(err))
		c.errorCount.Inc()
	}

	for _, endpointSubset := range endpoints.Subsets {
		for _, address := range endpointSubset.Addresses {
			hosts = append(hosts, fmt.Sprintf("%v:%v", address.IP, c.port))
		}
	}

	for _, host := range hosts {
		start := time.Now()

		conn, err := c.dialer.Dial("tcp", host)
		if err != nil {
			c.logger.Log("level", "error", "message", "could not dial host", "host", host, "stack", microerror.Stack(err))
			c.dialErrorCount.WithLabelValues(host).Inc()
			return
		}

		defer conn.Close()

		elapsed := time.Since(start)

		time.Sleep(100 * time.Millisecond)

		c.latencyHistogramVec.Add(host, elapsed.Seconds())
	}

	c.latencyHistogramVec.Ensure(hosts)

	for host, histogram := range c.latencyHistogramVec.Histograms() {
		ch <- prometheus.MustNewConstHistogram(
			c.latencyHistogramDesc,
			histogram.Count(), histogram.Sum(), histogram.Buckets(),
			host,
		)
	}
}
