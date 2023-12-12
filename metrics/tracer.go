package metrics

import (
	"net"
	"sync"
	"time"

	"github.com/quic-go/quic-go/logging"
	"go.k6.io/k6/js/modules"
	"go.k6.io/k6/metrics"
	k6metrics "go.k6.io/k6/metrics"
)

type HTTP3Metrics struct {
	HTTP3ReqDuration  *k6metrics.Metric
	HTTP3ReqSending   *k6metrics.Metric
	HTTP3ReqWaiting   *k6metrics.Metric
	HTTP3ReqReceiving *k6metrics.Metric
	HTTP3Reqs         *k6metrics.Metric
}

const (
	HTTP3ReqDurationName  = "http3_req_duration"
	HTTP3ReqSendingName   = "http3_req_sending"
	HTTP3ReqWaitingName   = "http3_req_waiting"
	HTTP3ReqReceivingName = "http3_req_receiving"
	HTTP3ReqsName         = "http3_reqs"
)

type streamMetrics struct {
	requestStart  time.Time
	requestFin    time.Time
	responseStart time.Time
	responseFin   time.Time
	receivedBytes int
	sentBytes     int
}
type metricHandler struct {
	wg      *sync.WaitGroup
	vu      modules.VU
	metrics *HTTP3Metrics
	streams map[int64]*streamMetrics
}

func RegisterMetrics(vu modules.VU) (*HTTP3Metrics, error) {
	var err error
	registry := vu.InitEnv().Registry
	m := &HTTP3Metrics{}

	m.HTTP3ReqDuration, err = registry.NewMetric(HTTP3ReqDurationName, metrics.Trend, metrics.Time)
	if err != nil {
		return m, err
	}
	m.HTTP3ReqReceiving, err = registry.NewMetric(HTTP3ReqReceivingName, metrics.Trend, metrics.Time)
	if err != nil {
		return m, err
	}
	m.HTTP3ReqWaiting, err = registry.NewMetric(HTTP3ReqWaitingName, metrics.Trend, metrics.Time)
	if err != nil {
		return m, err
	}
	m.HTTP3ReqSending, err = registry.NewMetric(HTTP3ReqSendingName, metrics.Trend, metrics.Time)
	if err != nil {
		return m, err
	}
	m.HTTP3Reqs, err = registry.NewMetric(HTTP3ReqsName, metrics.Counter)
	if err != nil {
		return m, err
	}

	return m, nil
}

func (mh *metricHandler) sendMetrics(streamID int64) {
	streamMetrics := mh.getStreamMetrics(streamID, false)
	if streamMetrics == nil {
		return
	}
	mh.streams[streamID] = nil
	http3ReqDuration := streamMetrics.responseFin.Sub(streamMetrics.requestStart)
	http3ReqSending := streamMetrics.requestFin.Sub(streamMetrics.requestStart)
	http3ReqWaiting := streamMetrics.responseStart.Sub(streamMetrics.requestFin)
	http3ReqReceiving := streamMetrics.responseFin.Sub(streamMetrics.responseStart)

	samples := k6metrics.ConnectedSamples{
		Samples: []k6metrics.Sample{
			{
				TimeSeries: metrics.TimeSeries{
					Metric: mh.metrics.HTTP3ReqDuration,
					Tags:   nil,
				},
				Metadata: nil,
				Time:     streamMetrics.responseFin,
				Value:    k6metrics.D(http3ReqDuration),
			},
			{
				TimeSeries: metrics.TimeSeries{
					Metric: mh.metrics.HTTP3ReqSending,
					Tags:   nil,
				},
				Metadata: nil,
				Time:     streamMetrics.requestFin,
				Value:    k6metrics.D(http3ReqSending),
			},
			{
				TimeSeries: metrics.TimeSeries{
					Metric: mh.metrics.HTTP3ReqWaiting,
					Tags:   nil,
				},
				Metadata: nil,
				Time:     streamMetrics.responseStart,
				Value:    k6metrics.D(http3ReqWaiting),
			},
			{
				TimeSeries: metrics.TimeSeries{
					Metric: mh.metrics.HTTP3ReqReceiving,
					Tags:   nil,
				},
				Metadata: nil,
				Time:     streamMetrics.responseFin,
				Value:    k6metrics.D(http3ReqReceiving),
			},
			{
				TimeSeries: metrics.TimeSeries{
					Metric: mh.metrics.HTTP3Reqs,
					Tags:   nil,
				},
				Metadata: nil,
				Time:     streamMetrics.responseFin,
				Value:    1,
			},
		}}
	mh.vu.State().Samples <- samples
	mh.wg.Done()
}

func (mh *metricHandler) ConnectionStarted(t time.Time) {
	//TODO: Handle it
}

func (mh *metricHandler) handleFramesReceived(frames []logging.Frame) {
	for _, frame := range frames {
		switch f := frame.(type) {
		case *logging.StreamFrame:
			{
				streamID := int64(f.StreamID)
				streamMetrics := mh.getStreamMetrics(streamID, false)
				if streamMetrics == nil {
					return
				}
				streamMetrics.receivedBytes += int(f.Length)
				if streamMetrics.responseStart.IsZero() {
					streamMetrics.responseStart = time.Now()
				}
				if f.Fin {
					streamMetrics.responseFin = time.Now()
					mh.sendMetrics(streamID)
				}
			}
		}
	}
}

func (mh *metricHandler) getStreamMetrics(streamID int64, createIfNotFound bool) *streamMetrics {
	now := time.Now()
	if m, ok := mh.streams[streamID]; ok || !createIfNotFound {
		return m
	}
	m := &streamMetrics{
		requestStart: now,
	}
	mh.streams[streamID] = m
	return m
}

func (mh *metricHandler) handleFramesSent(frames []logging.Frame) {
	for _, frame := range frames {
		switch f := frame.(type) {
		case *logging.StreamFrame:
			{
				streamID := int64(f.StreamID)
				streamMetrics := mh.getStreamMetrics(streamID, true)
				streamMetrics.sentBytes += int(f.Length)
				if f.Fin {
					streamMetrics.requestFin = time.Now()
				}
			}
		}
	}

}

func (mh *metricHandler) PacketReceived(bc logging.ByteCount, f []logging.Frame) {
	mh.handleFramesReceived(f)
	sample := k6metrics.Sample{
		TimeSeries: metrics.TimeSeries{
			Metric: mh.vu.State().BuiltinMetrics.DataReceived,
		},
		Time:  time.Now(),
		Value: float64(bc),
	}
	metrics.PushIfNotDone(mh.vu.Context(), mh.vu.State().Samples, sample)
}

func (mh *metricHandler) PacketSent(bc logging.ByteCount, f []logging.Frame) {
	mh.handleFramesSent(f)
	sample := k6metrics.Sample{
		TimeSeries: metrics.TimeSeries{
			Metric: mh.vu.State().BuiltinMetrics.DataSent,
		},
		Time:  time.Now(),
		Value: float64(bc),
	}
	metrics.PushIfNotDone(mh.vu.Context(), mh.vu.State().Samples, sample)
}

func NewTracer(vu modules.VU, http3Metrics *HTTP3Metrics, wg *sync.WaitGroup) *logging.ConnectionTracer {
	mh := &metricHandler{
		wg:      wg,
		vu:      vu,
		metrics: http3Metrics,
		streams: make(map[int64]*streamMetrics),
	}

	return &logging.ConnectionTracer{
		StartedConnection: func(local, remote net.Addr, srcConnID, destConnID logging.ConnectionID) {
			mh.ConnectionStarted(time.Now())
		},
		ReceivedLongHeaderPacket: func(eh *logging.ExtendedHeader, bc logging.ByteCount, e logging.ECN, f []logging.Frame) {
			mh.PacketReceived(bc, f)
		},
		ReceivedShortHeaderPacket: func(sh *logging.ShortHeader, bc logging.ByteCount, e logging.ECN, f []logging.Frame) {
			mh.PacketReceived(bc, f)
		},
		SentLongHeaderPacket: func(eh *logging.ExtendedHeader, bc logging.ByteCount, e logging.ECN, af *logging.AckFrame, f []logging.Frame) {
			mh.PacketSent(bc, f)
		},
		SentShortHeaderPacket: func(sh *logging.ShortHeader, bc logging.ByteCount, e logging.ECN, af *logging.AckFrame, f []logging.Frame) {
			mh.PacketSent(bc, f)
		},
	}
}
