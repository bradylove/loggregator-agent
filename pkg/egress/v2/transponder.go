package v2

import (
	"time"

	"code.cloudfoundry.org/go-loggregator/pulseemitter"
	"code.cloudfoundry.org/go-loggregator/rpc/loggregator_v2"
	"code.cloudfoundry.org/loggregator-agent/pkg/plumbing/batching"
)

type Nexter interface {
	TryNext() (*loggregator_v2.Envelope, bool)
}

type Writer interface {
	Write(msgs []*loggregator_v2.Envelope) error
}

// MetricClient creates new CounterMetrics to be emitted periodically.
type MetricClient interface {
	NewCounterMetric(name string, opts ...pulseemitter.MetricOption) pulseemitter.CounterMetric
}

type Transponder struct {
	nexter        Nexter
	writer        Writer
	tags          map[string]string
	batcher       *batching.V2EnvelopeBatcher
	batchSize     int
	batchInterval time.Duration
	droppedMetric pulseemitter.CounterMetric
	egressMetric  pulseemitter.CounterMetric
}

func NewTransponder(
	n Nexter,
	w Writer,
	tags map[string]string,
	batchSize int,
	batchInterval time.Duration,
	metricClient MetricClient,
) *Transponder {
	droppedMetric := metricClient.NewCounterMetric("dropped",
		pulseemitter.WithVersion(2, 0),
		pulseemitter.WithTags(map[string]string{"direction": "egress"}),
	)

	egressMetric := metricClient.NewCounterMetric("egress",
		pulseemitter.WithVersion(2, 0),
	)

	return &Transponder{
		nexter:        n,
		writer:        w,
		tags:          tags,
		droppedMetric: droppedMetric,
		egressMetric:  egressMetric,
		batchSize:     batchSize,
		batchInterval: batchInterval,
	}
}

func (t *Transponder) Start() {
	b := batching.NewV2EnvelopeBatcher(
		t.batchSize,
		t.batchInterval,
		batching.V2EnvelopeWriterFunc(t.write),
	)

	for {
		envelope, ok := t.nexter.TryNext()
		if !ok {
			b.Flush()
			time.Sleep(100 * time.Millisecond)
			continue
		}

		b.Write(envelope)
	}
}

func (t *Transponder) write(batch []*loggregator_v2.Envelope) {
	for _, e := range batch {
		t.addTags(e)
	}

	if err := t.writer.Write(batch); err != nil {
		// metric-documentation-v2: (loggregator.metron.dropped) Number of messages
		// dropped when failing to write to Dopplers v2 API
		t.droppedMetric.Increment(uint64(len(batch)))
		return
	}

	// metric-documentation-v2: (loggregator.metron.egress)
	// Number of messages written to Doppler's v2 API
	t.egressMetric.Increment(uint64(len(batch)))
}

func (t *Transponder) addTags(e *loggregator_v2.Envelope) {
	if e.DeprecatedTags == nil {
		e.DeprecatedTags = make(map[string]*loggregator_v2.Value)
	}

	// Move non-deprecated tags to deprecated tags. This is required
	// for backwards compatibility purposes and should be removed once
	// deprecated tags are fully removed.
	for k, v := range e.GetTags() {
		e.DeprecatedTags[k] = &loggregator_v2.Value{
			Data: &loggregator_v2.Value_Text{
				Text: v,
			},
		}
	}

	for k, v := range t.tags {
		if _, ok := e.DeprecatedTags[k]; !ok {
			e.DeprecatedTags[k] = &loggregator_v2.Value{
				Data: &loggregator_v2.Value_Text{
					Text: v,
				},
			}
		}
	}
}
