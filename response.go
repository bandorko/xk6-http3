package http3

import "go.k6.io/k6/lib/netext/httpext"

type Response struct {
	*httpext.Response `js:"-"`
	client            *Client
}
