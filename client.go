package http3

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"reflect"
	"strings"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/dop251/goja"
	"github.com/klauspost/compress/zstd"
	"go.k6.io/k6/js/common"
	k6http "go.k6.io/k6/js/modules/k6/http"
	"go.k6.io/k6/lib"
	"go.k6.io/k6/lib/netext/httpext"
	"go.k6.io/k6/lib/types"
	"gopkg.in/guregu/null.v3"
)

func (c *Client) getMethodClosure(method string) func(url goja.Value, args ...goja.Value) (*Response, error) {
	return func(url goja.Value, args ...goja.Value) (*Response, error) {
		c.moduleInstance.wg.Add(1)
		return c.Request(method, url, args...)
	}
}

// ErrHTTPForbiddenInInitContext is used when a http requests was made in the init context
var ErrHTTPForbiddenInInitContext = common.NewInitContextError("Making http requests in the init context is not supported")

// ErrBatchForbiddenInInitContext is used when batch was made in the init context
var ErrBatchForbiddenInInitContext = common.NewInitContextError("Using batch in the init context is not supported")

type Client struct {
	moduleInstance   *ModuleInstance
	responseCallback func(int) bool
	client           *http.Client
}

// Request makes an http request of the provided `method` and returns a corresponding response by
// taking goja.Values as arguments
func (c *Client) Request(method string, url goja.Value, args ...goja.Value) (*Response, error) {
	state := c.moduleInstance.vu.State()
	if state == nil {
		return nil, ErrHTTPForbiddenInInitContext
	}
	body, params := splitRequestArgs(args)

	req, err := c.parseRequest(method, url, body, params)
	if err != nil {
		return c.handleParseRequestError(err)
	}

	resp, err := c.makeRequest(c.moduleInstance.vu.Context(), state, req)
	if err != nil {
		return nil, err
	}
	c.processResponse(resp, req.ResponseType)
	return c.responseFromHTTPext(resp), nil
}

// processResponse stores the body as an ArrayBuffer if indicated by
// respType. This is done here instead of in httpext.readResponseBody to avoid
// a reverse dependency on js/common or goja.
func (c *Client) processResponse(resp *httpext.Response, respType httpext.ResponseType) {
	if respType == httpext.ResponseTypeBinary && resp.Body != nil {
		resp.Body = c.moduleInstance.vu.Runtime().NewArrayBuffer(resp.Body.([]byte))
	}
}

func (c *Client) responseFromHTTPext(resp *httpext.Response) *Response {
	return &Response{Response: resp, client: c}
}

func (c *Client) makeRequest(ctx context.Context, state *lib.State, preq *httpext.ParsedHTTPRequest) (*httpext.Response, error) {

	resp, err := c.client.Do(preq.Req)
	respReq := &httpext.Request{
		Method:  preq.Req.Method,
		URL:     preq.Req.URL.String(),
		Cookies: stdCookiesToHTTPRequestCookies(preq.Req.Cookies()),
		Headers: preq.Req.Header,
	}
	body, err := readResponseBody(c.moduleInstance.vu.State(), preq.ResponseType, resp, err)
	if err != nil {
		return nil, err
	}
	httpresp := httpext.NewResponse()
	httpresp.Request = respReq
	httpresp.Body = body
	httpresp.URL = resp.Request.URL.String()
	httpresp.Status = resp.StatusCode
	httpresp.StatusText = resp.Status
	httpresp.Proto = resp.Proto
	httpresp.Headers = make(map[string]string, len(resp.Header))
	for k, vs := range resp.Header {
		httpresp.Headers[k] = strings.Join(vs, ", ")
	}
	resCookies := resp.Cookies()
	httpresp.Cookies = make(map[string][]*httpext.HTTPCookie, len(resCookies))
	for _, c := range resCookies {
		httpresp.Cookies[c.Name] = append(httpresp.Cookies[c.Name], &httpext.HTTPCookie{
			Name:     c.Name,
			Value:    c.Value,
			Domain:   c.Domain,
			Path:     c.Path,
			HTTPOnly: c.HttpOnly,
			Secure:   c.Secure,
			MaxAge:   c.MaxAge,
			Expires:  c.Expires.UnixNano() / 1000000,
		})
	}

	return httpresp, nil
}

const responseDecompressionErrorCode uint32 = 1701

// Matches non-compliant io.Closer implementations (e.g. zstd.Decoder)
type ncloser interface {
	Close()
}

type readCloser struct {
	io.Reader
}

// Close readers with differing Close() implementations
func (r readCloser) Close() error {
	var err error
	switch v := r.Reader.(type) {
	case io.Closer:
		err = v.Close()
	case ncloser:
		v.Close()
	}
	return err
}

func readResponseBody(
	state *lib.State,
	respType httpext.ResponseType,
	resp *http.Response,
	respErr error,
) (interface{}, error) {
	if resp == nil || respErr != nil {
		return nil, respErr
	}

	if respType == httpext.ResponseTypeNone {
		_, err := io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			respErr = err
		}
		return nil, respErr
	}

	rc := &readCloser{resp.Body}
	// Ensure that the entire response body is read and closed, e.g. in case of decoding errors
	defer func(respBody io.ReadCloser) {
		_, _ = io.Copy(io.Discard, respBody)
		_ = respBody.Close()
	}(resp.Body)

	if (resp.StatusCode >= 100 && resp.StatusCode <= 199) || // 1xx
		resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotModified {
		// for all three of this status code there is always no content
		// https://www.rfc-editor.org/rfc/rfc9110.html#section-6.4.1-8
		// this also prevents trying to read
		return nil, nil //nolint:nilnil
	}
	contentEncodings := strings.Split(resp.Header.Get("Content-Encoding"), ",")
	// Transparently decompress the body if it's has a content-encoding we
	// support. If not, simply return it as it is.
	for i := len(contentEncodings) - 1; i >= 0; i-- {
		contentEncoding := strings.TrimSpace(contentEncodings[i])
		if compression, err := httpext.CompressionTypeString(contentEncoding); err == nil {
			var decoder io.Reader
			var err error
			switch compression {
			case httpext.CompressionTypeDeflate:
				decoder, err = zlib.NewReader(rc)
			case httpext.CompressionTypeGzip:
				decoder, err = gzip.NewReader(rc)
			case httpext.CompressionTypeZstd:
				decoder, err = zstd.NewReader(rc)
			case httpext.CompressionTypeBr:
				decoder = brotli.NewReader(rc)
			default:
				// We have not implemented a compression ... :(
				err = fmt.Errorf(
					"unsupported compression type %s - this is a bug in k6, please report it",
					compression,
				)
			}
			if err != nil {
				return nil, err
			}
			rc = &readCloser{decoder}
		}
	}

	buf := state.BufferPool.Get()
	defer state.BufferPool.Put(buf)
	_, err := io.Copy(buf, rc.Reader)
	if err != nil {
		respErr = err
	}

	err = rc.Close()
	if err != nil && respErr == nil { // Don't overwrite previous errors
		respErr = err
	}

	var result interface{}
	// Binary or string
	switch respType {
	case httpext.ResponseTypeText:
		result = buf.String()
	case httpext.ResponseTypeBinary:
		// Copy the data to a new slice before we return the buffer to the pool,
		// because buf.Bytes() points to the underlying buffer byte slice.
		// The ArrayBuffer wrapping will be done in the js/modules/k6/http
		// package to avoid a reverse dependency, since it depends on goja.
		binData := make([]byte, buf.Len())
		copy(binData, buf.Bytes())
		result = binData
	default:
		respErr = fmt.Errorf("unknown responseType %s", respType)
	}

	return result, respErr
}

func splitRequestArgs(args []goja.Value) (body interface{}, params goja.Value) {
	if len(args) > 0 {
		body = args[0].Export()
	}
	if len(args) > 1 {
		params = args[1]
	}
	return body, params
}

func (c *Client) handleParseRequestError(err error) (*Response, error) {
	state := c.moduleInstance.vu.State()

	if state.Options.Throw.Bool {
		return nil, err
	}
	state.Logger.WithField("error", err).Warn("Request Failed")
	r := httpext.NewResponse()
	r.Error = err.Error()
	var k6e httpext.K6Error
	if errors.As(err, &k6e) {
		r.ErrorCode = int(k6e.Code)
	}
	return &Response{Response: r, client: c}, nil
}

// TODO: break this function up
//
//nolint:gocyclo, cyclop, funlen, gocognit
func (c *Client) parseRequest(
	method string, reqURL, body interface{}, params goja.Value,
) (*httpext.ParsedHTTPRequest, error) {
	rt := c.moduleInstance.vu.Runtime()
	state := c.moduleInstance.vu.State()
	if state == nil {
		return nil, ErrHTTPForbiddenInInitContext
	}

	if urlJSValue, ok := reqURL.(goja.Value); ok {
		reqURL = urlJSValue.Export()
	}
	u, err := httpext.ToURL(reqURL)
	if err != nil {
		return nil, err
	}

	result := &httpext.ParsedHTTPRequest{
		URL: &u,
		Req: &http.Request{
			Method: method,
			URL:    u.GetURL(),
			Header: make(http.Header),
		},
		Timeout:          60 * time.Second,
		Throw:            state.Options.Throw.Bool,
		Redirects:        state.Options.MaxRedirects,
		Cookies:          make(map[string]*httpext.HTTPRequestCookie),
		ResponseCallback: c.responseCallback,
		TagsAndMeta:      c.moduleInstance.vu.State().Tags.GetCurrentValues(),
	}

	if state.Options.DiscardResponseBodies.Bool {
		result.ResponseType = httpext.ResponseTypeNone
	} else {
		result.ResponseType = httpext.ResponseTypeText
	}

	formatFormVal := func(v interface{}) string {
		// TODO: handle/warn about unsupported/nested values
		return fmt.Sprintf("%v", v)
	}

	handleObjectBody := func(data map[string]interface{}) error {
		if !requestContainsFile(data) {
			bodyQuery := make(url.Values, len(data))
			for k, v := range data {
				if arr, ok := v.([]interface{}); ok {
					for _, el := range arr {
						bodyQuery.Add(k, formatFormVal(el))
					}
					continue
				}
				bodyQuery.Set(k, formatFormVal(v))
			}
			result.Body = bytes.NewBufferString(bodyQuery.Encode())
			result.Req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			return nil
		}

		// handling multipart request
		result.Body = &bytes.Buffer{}
		mpw := multipart.NewWriter(result.Body)

		// For parameters of type common.FileData, created with open(file, "b"),
		// we write the file boundary to the body buffer.
		// Otherwise parameters are treated as standard form field.
		for k, v := range data {
			switch ve := v.(type) {
			case FileData:
				// writing our own part to handle receiving
				// different content-type than the default application/octet-stream
				h := make(textproto.MIMEHeader)
				escapedFilename := escapeQuotes(ve.Filename)
				h.Set("Content-Disposition",
					fmt.Sprintf(`form-data; name="%s"; filename="%s"`,
						k, escapedFilename))
				h.Set("Content-Type", ve.ContentType)

				// this writer will be closed either by the next part or
				// the call to mpw.Close()
				fw, err := mpw.CreatePart(h)
				if err != nil {
					return err
				}

				if _, err := fw.Write(ve.Data); err != nil {
					return err
				}
			default:
				fw, err := mpw.CreateFormField(k)
				if err != nil {
					return err
				}

				if _, err := fw.Write([]byte(formatFormVal(v))); err != nil {
					return err
				}
			}
		}

		if err := mpw.Close(); err != nil {
			return err
		}

		result.Req.Header.Set("Content-Type", mpw.FormDataContentType())
		return nil
	}

	if body != nil {
		switch data := body.(type) {
		case map[string]goja.Value:
			// TODO: fix forms submission and serialization in k6/html before fixing this..
			newData := map[string]interface{}{}
			for k, v := range data {
				newData[k] = v.Export()
			}
			if err := handleObjectBody(newData); err != nil {
				return nil, err
			}
		case goja.ArrayBuffer:
			result.Body = bytes.NewBuffer(data.Bytes())
		case map[string]interface{}:
			if err := handleObjectBody(data); err != nil {
				return nil, err
			}
		case string:
			result.Body = bytes.NewBufferString(data)
		case []byte:
			result.Body = bytes.NewBuffer(data)
		default:
			return nil, fmt.Errorf("unknown request body type %T", body)
		}
	}

	result.Req.Header.Set("User-Agent", state.Options.UserAgent.String)

	if state.CookieJar != nil {
		result.ActiveJar = state.CookieJar
	}

	// TODO: ditch goja.Value, reflections and Object and use a simple go map and type assertions?
	if params != nil && !goja.IsUndefined(params) && !goja.IsNull(params) {
		params := params.ToObject(rt)
		for _, k := range params.Keys() {
			switch k {
			case "cookies":
				cookiesV := params.Get(k)
				if goja.IsUndefined(cookiesV) || goja.IsNull(cookiesV) {
					continue
				}
				cookies := cookiesV.ToObject(rt)
				if cookies == nil {
					continue
				}
				for _, key := range cookies.Keys() {
					cookieV := cookies.Get(key)
					if goja.IsUndefined(cookieV) || goja.IsNull(cookieV) {
						continue
					}
					switch cookieV.ExportType() {
					case reflect.TypeOf(map[string]interface{}{}):
						result.Cookies[key] = &httpext.HTTPRequestCookie{Name: key, Value: "", Replace: false}
						cookie := cookieV.ToObject(rt)
						for _, attr := range cookie.Keys() {
							switch strings.ToLower(attr) {
							case "replace":
								result.Cookies[key].Replace = cookie.Get(attr).ToBoolean()
							case "value":
								result.Cookies[key].Value = cookie.Get(attr).String()
							}
						}
					default:
						result.Cookies[key] = &httpext.HTTPRequestCookie{Name: key, Value: cookieV.String(), Replace: false}
					}
				}
			case "headers":
				headersV := params.Get(k)
				if goja.IsUndefined(headersV) || goja.IsNull(headersV) {
					continue
				}
				headers := headersV.ToObject(rt)
				if headers == nil {
					continue
				}
				for _, key := range headers.Keys() {
					str := headers.Get(key).String()
					if strings.ToLower(key) == "host" {
						result.Req.Host = str
					}
					result.Req.Header.Set(key, str)
				}
			case "jar":
				jarV := params.Get(k)
				if goja.IsUndefined(jarV) || goja.IsNull(jarV) {
					continue
				}
				switch v := jarV.Export().(type) {
				case *k6http.CookieJar:
					result.ActiveJar = v.Jar
				}
			case "compression":
				algosString := strings.TrimSpace(params.Get(k).ToString().String())
				if algosString == "" {
					continue
				}
				algos := strings.Split(algosString, ",")
				var err error
				result.Compressions = make([]httpext.CompressionType, len(algos))
				for index, algo := range algos {
					algo = strings.TrimSpace(algo)
					result.Compressions[index], err = httpext.CompressionTypeString(algo)
					if err != nil {
						return nil, fmt.Errorf("unknown compression algorithm %s, supported algorithms are %s",
							algo, httpext.CompressionTypeValues())
					}
				}
			case "redirects":
				result.Redirects = null.IntFrom(params.Get(k).ToInteger())
			case "tags":
				if err := common.ApplyCustomUserTags(rt, &result.TagsAndMeta, params.Get(k)); err != nil {
					return nil, fmt.Errorf("invalid HTTP request metric tags: %w", err)
				}
			case "auth":
				result.Auth = params.Get(k).String()
			case "timeout":
				t, err := types.GetDurationValue(params.Get(k).Export())
				if err != nil {
					return nil, fmt.Errorf("invalid timeout value: %w", err)
				}
				result.Timeout = t
			case "throw":
				result.Throw = params.Get(k).ToBoolean()
			case "responseType":
				responseType, err := httpext.ResponseTypeString(params.Get(k).String())
				if err != nil {
					return nil, err
				}
				result.ResponseType = responseType
			case "responseCallback":
				v := params.Get(k).Export()
				if v == nil {
					result.ResponseCallback = nil
				} else if c, ok := v.(*expectedStatuses); ok {
					result.ResponseCallback = c.match
				} else {
					return nil, fmt.Errorf("unsupported responseCallback")
				}
			}
		}
	}

	if result.ActiveJar != nil {
		httpext.SetRequestCookies(result.Req, result.ActiveJar, result.Cookies)
	}

	return result, nil
}

func requestContainsFile(data map[string]interface{}) bool {
	for _, v := range data {
		switch v.(type) {
		case FileData:
			return true
		}
	}
	return false
}

// FileData represents a binary file requiring multipart request encoding
type FileData struct {
	Data        []byte
	Filename    string
	ContentType string
}

var quoteEscaper = strings.NewReplacer("\\", "\\\\", `"`, "\\\"")

func escapeQuotes(s string) string {
	return quoteEscaper.Replace(s)
}

// expectedStatuses is specifically totally unexported so it can't be used for anything else but
// SetResponseCallback and nothing can be done from the js side to modify it or make an instance of
// it except using ExpectedStatuses
type expectedStatuses struct {
	minmax [][2]int
	exact  []int
}

func (e expectedStatuses) match(status int) bool {
	for _, v := range e.exact {
		if v == status {
			return true
		}
	}

	for _, v := range e.minmax {
		if v[0] <= status && status <= v[1] {
			return true
		}
	}
	return false
}

func stdCookiesToHTTPRequestCookies(cookies []*http.Cookie) map[string][]*httpext.HTTPRequestCookie {
	result := make(map[string][]*httpext.HTTPRequestCookie, len(cookies))
	for _, cookie := range cookies {
		result[cookie.Name] = append(result[cookie.Name],
			&httpext.HTTPRequestCookie{Name: cookie.Name, Value: cookie.Value})
	}
	return result
}
