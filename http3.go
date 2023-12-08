package http3ext

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log"
	"net/http"

	"github.com/dop251/goja"
	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
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
	mi := &ModuleInstance{
		vu: vu,
		client: &http.Client{
			Transport: createHTTP3RoundTripper(false),
		},
		exports: rt.NewObject(),
	}

	mustExport := func(name string, value interface{}) {
		if err := mi.exports.Set(name, value); err != nil {
			common.Throw(rt, err)
		}
	}

	mustExport("get", func(url goja.Value, args ...goja.Value) (string, error) {
		resp, err := mi.client.Get(url.String())
		if err != nil {
			return "", err
		}
		body := fmt.Sprintf("%#v", resp)
		return body, nil
	})
	return mi
}

func createHTTP3RoundTripper(insecure bool) *http3.RoundTripper {
	var qconf quic.Config
	pool, err := x509.SystemCertPool()
	if err != nil {
		log.Fatal(err)
	}
	roundTripper := &http3.RoundTripper{
		TLSClientConfig: &tls.Config{
			RootCAs:            pool,
			InsecureSkipVerify: insecure,
			//KeyLogWriter:       keyLog,
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
