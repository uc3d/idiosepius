package main

import (
	"fmt"
	"html/template"
	"math/rand"
	"net/http"
	"time"
)

func webserverMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "<html><body><div id=content>")
		fmt.Fprintln(w, "</div>")
		fmt.Fprintln(w, `<script>
window.addEventListener("load", function(){
	window.setInterval(function(){
		var xhr = new XMLHttpRequest();
		xhr.onreadystatechange = function() {
			if (xhr.readyState === 4){
				document.getElementById('content').innerHTML = xhr.responseText;
			}
		};
		xhr.open('GET', '/stats');
		xhr.send();
	}, 500)
});
</script>`)
		fmt.Fprintln(w, "</body></html>")
	})
	mux.HandleFunc("/stats", func(w http.ResponseWriter, r *http.Request) {
		type PrinterStats struct {
			Hotend string
			Bed    string
		}
		homeTpl.Execute(w, struct {
			Connection   string
			PrinterStats PrinterStats
			Now          string
		}{"/dev/ttyS0@115220", PrinterStats{Hotend: fmt.Sprint(198+rand.Intn(4), "C"), Bed: fmt.Sprint(59+rand.Intn(2), "C")}, time.Now().String()})
	})
	return mux
}

var homeTpl = template.Must(template.New("homeTpl").Parse(`
<table width=50% align=center cellpadding=1 border=1>
	<thead>
		<th colspan=2>muPrint</th>
	</thead>
	<tbody><tr>
		<td>
		Serial Connection:<br>{{- .Connection -}}
		</td>
		<td>
		Hotend: {{- .PrinterStats.Hotend -}}<br/>
		Bed: {{- .PrinterStats.Bed -}}<br/>
		</td>
	</tr></tbody>
	<tfoot>
	<tr>
		<td colspan=2 align=center>{{ .Now }}</td>
	</tr>
	</tfoot>
</table>
`))
