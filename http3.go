package http3

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/dop251/goja"
	"github.com/quic-go/quic-go"
	quichttp3 "github.com/quic-go/quic-go/http3"
	"github.com/quic-go/quic-go/logging"
	"github.com/quic-go/quic-go/qlog"
	"go.k6.io/k6/event"
	"go.k6.io/k6/js/common"
	"go.k6.io/k6/js/modules"
)

func init() {
	modules.Register("k6/x/http3", New())
}

type (
	RootModule struct{}

	ModuleInstance struct {
		vu      modules.VU
		metrics *HTTP3Metrics
		client  *Client
		exports *goja.Object
	}
)

var (
	_ modules.Instance = &ModuleInstance{}
	_ modules.Module   = &RootModule{}
)

func New() *RootModule {
	return &RootModule{}
}

func (*RootModule) NewModuleInstance(vu modules.VU) modules.Instance {
	rt := vu.Runtime()
	sub, ch := vu.Events().Global.Subscribe(event.TestEnd)

	metrics, err := RegisterMetrics(vu)
	if err != nil {
		log.Fatal(err)
	}

	mi := &ModuleInstance{
		vu:      vu,
		metrics: metrics,
		exports: rt.NewObject(),
	}

	go func() {
		ev := <-ch
		if mi.client != nil {
			mi.client.client.Transport.(*quichttp3.RoundTripper).Close()
		}
		ev.Done()
		vu.Events().Global.Unsubscribe(sub)
	}()

	mustExport := func(name string, value interface{}) {
		if err := mi.exports.Set(name, value); err != nil {
			common.Throw(rt, err)
		}
	}

	getMethodClosure := func(method string) func(url goja.Value, args ...goja.Value) (*Response, error) {
		return func(url goja.Value, args ...goja.Value) (*Response, error) {
			return mi.getClient().Request(method, url, args...)
		}
	}
	mustExport("get", func(url goja.Value, args ...goja.Value) (*Response, error) {
		// http3.get(url, params) doesn't have a body argument, so we add undefined
		// as the third argument to http.request(method, url, body, params)
		args = append([]goja.Value{goja.Undefined()}, args...)
		return mi.getClient().Request(http.MethodGet, url, args...)
	})
	mustExport("head", func(url goja.Value, args ...goja.Value) (*Response, error) {
		// http3.head(url, onStreamCompletedImplparams) doesn't have a body argument, so we add undefined
		// as the third argument to http.request(method, url, body, params)
		args = append([]goja.Value{goja.Undefined()}, args...)
		return mi.getClient().Request(http.MethodHead, url, args...)
	})
	mustExport("post", getMethodClosure(http.MethodPost))
	mustExport("put", getMethodClosure(http.MethodPut))
	mustExport("patch", getMethodClosure(http.MethodPatch))
	mustExport("del", getMethodClosure(http.MethodDelete))
	mustExport("options", getMethodClosure(http.MethodOptions))
	mustExport("request", func(method string, url goja.Value, args ...goja.Value) (*Response, error) {
		return mi.getClient().Request(method, url, args...)
	})
	return mi
}

func (mi *ModuleInstance) getClient() *Client {
	if mi.client == nil {
		mi.client = &Client{
			moduleInstance: mi,
			client: &http.Client{
				CheckRedirect: func(req *http.Request, via []*http.Request) error {
					return http.ErrUseLastResponse
				},
				Transport: mi.createHTTP3RoundTripper(mi.vu.State().TLSConfig.InsecureSkipVerify),
			},
		}
	}
	return mi.client
}

func (mi *ModuleInstance) createHTTP3RoundTripper(insecure bool) *quichttp3.RoundTripper {
	qconf := quic.Config{

		Tracer: func(ctx context.Context, p logging.Perspective, connID quic.ConnectionID) *logging.ConnectionTracer {
			tracers := make([]*logging.ConnectionTracer, 0)
			tracers = append(tracers, NewTracer(mi.vu, mi.metrics))
			if os.Getenv("HTTP3_QLOG") == "1" {
				role := "server"
				if p == logging.PerspectiveClient {
					role = "client"
				}
				filename := fmt.Sprintf("./log_%s_%s.qlog", connID, role)
				f, _ := os.Create(filename)
				// TODO: handle the error
				tracers = append(tracers, qlog.NewConnectionTracer(f, p, connID))
			}
			return logging.NewMultiplexedConnectionTracer(tracers...)
		},
	}

	pool, err := x509.SystemCertPool()
	if err != nil {
		log.Fatal(err)
	}
	roundTripper := &quichttp3.RoundTripper{
		TLSClientConfig: &tls.Config{
			RootCAs:            pool,
			InsecureSkipVerify: insecure,
		},
		QuicConfig: &qconf,
	}
	return roundTripper
}

func (mi *ModuleInstance) Exports() modules.Exports {
	return modules.Exports{
		Default: mi.exports,
	}
}
