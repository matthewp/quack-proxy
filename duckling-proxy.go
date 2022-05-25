package main

import (
	"context"
	"crypto/tls"
	_ "embed"
	"html/template"
	"os/signal"
	"strconv"

	"git.sr.ht/~adnano/go-gemini"
	"git.sr.ht/~adnano/go-gemini/certificate"
	"github.com/LukeEmmet/html2gemini"
	flag "github.com/spf13/pflag"

	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

var version = "0.2.1"

type WebPipeHandler struct {
}

var (
	citationStart     = flag.IntP("citationStart", "s", 1, "Start citations from this index")
	citationMarkers   = flag.BoolP("citationMarkers", "m", false, "Use footnote style citation markers")
	numberedLinks     = flag.BoolP("numberedLinks", "n", false, "Number the links")
	prettyTables      = flag.BoolP("prettyTables", "r", false, "Pretty tables - works with most simple tables")
	emitImagesAsLinks = flag.BoolP("emitImagesAsLinks", "e", false, "Emit links to included images")
	linkEmitFrequency = flag.IntP("linkEmitFrequency", "l", 2, "Emit gathered links through the document after this number of paragraphs")
	serverCert        = flag.StringP("serverCert", "c", "", "serverCert path. ")
	serverKey         = flag.StringP("serverKey", "k", "", "serverKey path. ")
	userAgent         = flag.StringP("userAgent", "u", "", "User agent for HTTP requests\n")
	maxDownloadTime   = flag.IntP("maxDownloadTime", "t", 10, "Max download time (s)\n")
	maxConnectTime    = flag.IntP("maxConnectTime", "T", 5, "Max connect time (s)\n")
	port              = flag.IntP("port", "p", 1965, "Server port")
	address           = flag.StringP("address", "a", "127.0.0.1", "Bind to address\n")
	unfiltered        = flag.BoolP("unfiltered", "", false, "Do not filter text/html to text/gemini")
	verFlag           = flag.BoolP("version", "v", false, "Find out what version of Duckling Proxy you're running")
)

func fatal(format string, a ...interface{}) {
	urlError(format, a...)
	os.Exit(1)
}

func urlError(format string, a ...interface{}) {
	format = "Error: " + strings.TrimRight(format, "\n") + "\n"
	fmt.Fprintf(os.Stderr, format, a...)
}

func info(format string, a ...interface{}) {
	format = "Info: " + strings.TrimRight(format, "\n") + "\n"
	fmt.Fprintf(os.Stderr, format, a...)
}

func check(e error) {
	if e != nil {
		panic(e)
		os.Exit(1)
	}
}

func htmlToGmi(inputHtml string) (string, error) {

	//convert html to gmi
	options := html2gemini.NewOptions()
	options.PrettyTables = *prettyTables
	options.CitationStart = *citationStart
	options.LinkEmitFrequency = *linkEmitFrequency
	options.CitationMarkers = *citationMarkers
	options.NumberedLinks = *numberedLinks
	options.EmitImagesAsLinks = *emitImagesAsLinks

	//dont use an extra line to separate header from body, but
	//do separate each row visually
	options.PrettyTablesOptions.HeaderLine = false
	options.PrettyTablesOptions.RowLine = true

	//pretty tables option is somewhat experimental
	//and the column positions not always correct
	//so use invisible borders of spaces for now
	options.PrettyTablesOptions.CenterSeparator = " "
	options.PrettyTablesOptions.ColumnSeparator = " "
	options.PrettyTablesOptions.RowSeparator = " "

	ctx := html2gemini.NewTraverseContext(*options)

	return html2gemini.FromString(inputHtml, *ctx)

}

//func (h WebPipeHandler) Handle(r gemini.Request) *gemini.Response {
func (h WebPipeHandler) Handle(ctx context.Context, w gemini.ResponseWriter, r *gemini.Request) {
	fmt.Printf("URL %v\n", r)

	url := r.URL.String()
	if r.URL.Scheme != "http" && r.URL.Scheme != "https" {
		//any other schemes are not implemented by this proxy
		w.WriteHeader(53, "Scheme not supported: "+r.URL.Scheme)
		return
		//return &gemini.Response{53, "Scheme not supported: " + r.URL.Scheme, nil, nil}
	}

	info("Retrieve: %s", r.URL.String())

	//see https://medium.com/@nate510/don-t-use-go-s-default-http-client-4804cb19f779
	//also https://gist.github.com/ijt/950790/fca88967337b9371bb6f7155f3304b3ccbf3946f

	connectTimeout := time.Second * time.Duration(*maxConnectTime)
	clientTimeout := time.Second * time.Duration(*maxDownloadTime)

	//create custom transport with timeout
	var netTransport = &http.Transport{
		Dial: (&net.Dialer{
			Timeout: connectTimeout,
		}).Dial,
		TLSHandshakeTimeout: connectTimeout,
	}

	//create custom client with timeout
	var netClient = &http.Client{
		Timeout:   clientTimeout,
		Transport: netTransport,
	}

	//fmt.Println("making request")
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		w.WriteHeader(43, "Could not connect to remote HTTP host")
		return
		//return &gemini.Response{43, "Could not connect to remote HTTP host", nil, nil}
	}

	//set user agent if specified
	if *userAgent != "" {
		req.Header.Add("User-Agent", *userAgent)
	}

	response, err := netClient.Do(req)
	if err != nil {
		w.WriteHeader(43, "Remote host did not respond with valid HTTP")
		return
		//return &gemini.Response{43, "Remote host did not respond with valid HTTP", nil, nil}
	}

	defer response.Body.Close()

	//final response (may have redirected)
	if url != response.Request.URL.String() {
		//notify of target location on stderr
		//see https://stackoverflow.com/questions/16784419/in-golang-how-to-determine-the-final-url-after-a-series-of-redirects
		info("Redirected to: %s", response.Request.URL.String())

		//tell the client to get it from a different location otherwise the client
		//wont know the baseline for link refs
		w.WriteHeader(30, response.Request.URL.String())
		return

		//return &gemini.Response{30, response.Request.URL.String(), nil, nil}
	}

	contents, err := ioutil.ReadAll(response.Body)
	if err != nil {
		abandonMsg := fmt.Sprintf("Download abandoned after %d seconds: %s", *maxDownloadTime, response.Request.URL.String())
		info(abandonMsg)
		w.WriteHeader(43, abandonMsg)
		return
		//return &gemini.Response{43, abandonMsg, nil, nil}
	}

	if response.StatusCode == 200 {
		contentType := response.Header.Get("Content-Type")

		info("Content-Type: %s", contentType)

		var body io.ReadCloser
		if !*unfiltered && strings.Contains(contentType, "text/html") {

			info("Converting to text/gemini: %s", r.URL.String())

			//translate html to gmi
			gmi, err := htmlToGmi(string(contents))

			if err != nil {
				w.WriteHeader(42, "HTML to GMI conversion failure")
				return
				//return &gemini.Response{42, "HTML to GMI conversion failure", nil, nil}
			}

			//add a footer to communicate that the content was filtered and not original
			//also the link provides a clickable link that the user can activate to launch a browser, depending on their client
			//behaviour (e.g. Ctrl-Click or similar)
			footer := ""
			footer += "\n\n──────────────────── 🦆 ──────────────────── 🦆 ──────────────────── \n\n"
			footer += "Web page filtered and simplified by Duckling Proxy v" + version + ". To view the original content, open the page in your system web browser.\n"
			footer += "=> " + r.URL.String() + " Source page \n"

			body = ioutil.NopCloser(strings.NewReader(string(gmi) + footer))

			contentType = "text/gemini"

		} else {
			//let everything else through with the same content type
			body = ioutil.NopCloser(strings.NewReader(string(contents)))
		}

		w.WriteHeader(20, contentType)
		io.Copy(w, body)
		return
		//return &gemini.Response{20, contentType, body, nil}

	} else if response.StatusCode == 404 {
		w.WriteHeader(51, "Not found")
		return
		//return &gemini.Response{51, "Not found", nil, nil}
	} else {
		w.WriteHeader(50, "Failure: HTTP status: "+response.Status)
		return
		//return &gemini.Response{50, "Failure: HTTP status: " + response.Status, nil, nil}
	}
}

//go:embed index.gmi.tmpl
var indextmpl string

func main() {
	flag.Parse()

	if *verFlag {
		fmt.Println("Duckling Proxy v" + version)
		return
	}

	handler := WebPipeHandler{}

	info("Starting Duckling Proxy v%s on %s port: %d", version, *address, *port)

	certificates := &certificate.Store{}
	var scope string = "*"
	certificates.Register(scope)

	pubkeybytes, err := ioutil.ReadFile(*serverCert)
	if err != nil {
		log.Fatal(err)
	}
	privkeybytes, err := ioutil.ReadFile(*serverKey)
	if err != nil {
		log.Fatal(err)
	}
	cert, err := tls.X509KeyPair(pubkeybytes, privkeybytes)
	if err != nil {
		log.Fatal(err)
	}
	certificates.Add(scope, cert)

	mux := &gemini.Mux{}
	index, err := template.New("index").Parse(indextmpl)
	if err != nil {
		log.Fatal(err)
	}
	mux.HandleFunc("/", func(ctx context.Context, w gemini.ResponseWriter, r *gemini.Request) {
		w.WriteHeader(gemini.StatusSuccess, "text/gemini")
		index.Execute(w, nil)
	})

	mux.HandleFunc("/view", func(ctx context.Context, w gemini.ResponseWriter, r *gemini.Request) {
		if r.URL.RawQuery == "" {
			w.WriteHeader(gemini.StatusInput, "")
		}
		query := r.URL.Query()
		for url := range query {
			nr, err := gemini.NewRequest(url)
			if err != nil {
				w.WriteHeader(gemini.StatusTemporaryFailure, "")
				w.Write([]byte(err.Error()))
				return
			}
			handler.Handle(ctx, w, nr)
			break
		}
	})

	baseHandler := gemini.HandlerFunc(func(ctx context.Context, w gemini.ResponseWriter, r *gemini.Request) {
		if r.URL.Scheme == "gemini" {
			mux.ServeGemini(ctx, w, r)
		} else {
			handler.Handle(ctx, w, r)
		}
	})

	server := &gemini.Server{
		Addr:           *address + ":" + strconv.Itoa(*port),
		Handler:        gemini.LoggingMiddleware(baseHandler),
		ReadTimeout:    30 * time.Second,
		WriteTimeout:   1 * time.Minute,
		GetCertificate: certificates.Get,
	}

	// Listen for interrupt signal
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)

	errch := make(chan error)
	go func() {
		ctx := context.Background()
		errch <- server.ListenAndServe(ctx)
	}()

	select {
	case err := <-errch:
		log.Fatal(err)
	case <-c:
		// Shutdown the server
		log.Println("Shutting down...")
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		err := server.Shutdown(ctx)
		if err != nil {
			log.Fatal(err)
		}
	}

	//err := gemini.ListenAndServe(*address+":"+strconv.Itoa(*port), *serverCert, *serverKey, handler)

	//if err != nil {
	//	log.Fatal(err)
	//}
}
