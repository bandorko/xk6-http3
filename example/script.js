import http3 from 'k6/x/http3'
import http from 'k6/http'
import {sleep} from 'k6'

function doHTTP(){
   let resp = http.get("https://www.google.com/")
   //console.log(resp);
}

function doHTTP3(){
   let resp = http3.get("https://www.google.com/")
   //console.log(resp);
}

export default function(){
  doHTTP3()
}

