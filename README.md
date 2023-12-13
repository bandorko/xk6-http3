# xk6-http3

This is an experiment to implement http3 client as an extension for k6.

## Build
```
xk6 build --with github.com/bandorko/xk6-http3
```

## Usage
Usage of the extension is similar to the [k6/http](https://grafana.com/docs/k6/latest/javascript-api/k6-http/) extension


### Example
```
import http3 from 'k6/x/http3'

export default function(){
  let resp = http3.get("https://www.google.com/")
  console.log(resp);
}
```

## QLOG
You can generate the http3 qlog file with the HTTP3_QLOG=1 environment variable