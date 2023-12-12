package http3ext

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log"
	"net/http"
	"sync"

	"github.com/bandorko/xk6-http3/metrics"
	"github.com/dop251/goja"
	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/quic-go/logging"
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
		wg      *sync.WaitGroup
		vu      modules.VU
		metrics *metrics.HTTP3Metrics
		client  *http.Client
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

	metrics, err := metrics.RegisterMetrics(vu)
	if err != nil {
		log.Fatal(err)
	}

	mi := &ModuleInstance{
		vu:      vu,
		wg:      &sync.WaitGroup{},
		metrics: metrics,
		exports: rt.NewObject(),
	}

	go func() {
		ev := <-ch
		mi.wg.Wait()
		ev.Done()
		vu.Events().Global.Unsubscribe(sub)
	}()

	mustExport := func(name string, value interface{}) {
		if err := mi.exports.Set(name, value); err != nil {
			common.Throw(rt, err)
		}
	}

	mustExport("get", func(url goja.Value, args ...goja.Value) (string, error) {
		mi.wg.Add(1)
		resp, err := mi.getClient().Get(url.String())
		if err != nil {
			return "", err
		}
		body := fmt.Sprintf("%#v", resp)
		return body, nil
	})
	return mi
}

func (mi *ModuleInstance) getClient() *http.Client {
	if mi.client == nil {
		mi.client = &http.Client{
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				fmt.Println("Redirect:", req.URL, via[0].URL)
				return http.ErrUseLastResponse
			},
			Transport: mi.createHTTP3RoundTripper(false),
		}
	}
	return mi.client
}

func (mi *ModuleInstance) createHTTP3RoundTripper(insecure bool) *http3.RoundTripper {
	qconf := quic.Config{

		Tracer: func(ctx context.Context, p logging.Perspective, connID quic.ConnectionID) *logging.ConnectionTracer {
			/*role := "server"
			if p == logging.PerspectiveClient {
				role = "client"
			}
			filename := fmt.Sprintf("./log_%s_%s.qlog", connID, role)
			f, err := os.Create(filename)
			fmt.Println(err)
			// handle the error*/
			return logging.NewMultiplexedConnectionTracer(metrics.NewTracer(mi.vu, mi.metrics, mi.wg) /*, qlog.NewConnectionTracer(f, p, connID)*/)
		},
	}

	pool, err := x509.SystemCertPool()
	if err != nil {
		log.Fatal(err)
	}
	roundTripper := &http3.RoundTripper{
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
