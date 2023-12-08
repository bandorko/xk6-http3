import http3 from 'k6/x/http3'

export default function(){
   var resp = http3.get("https://google.com")
   console.log(resp);
}