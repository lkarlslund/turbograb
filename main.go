package main

import (
	"encoding/json"
	"flag"
	"io/ioutil"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/schollz/progressbar/v3"
	"github.com/valyala/fasthttp"
)

type result struct {
	Site    string
	Body    string
	Headers map[string][]string
	Code    int
}

type logger struct {
}

func (l logger) Errorf(format string, v ...interface{}) {}
func (l logger) Warnf(format string, v ...interface{})  {}
func (l logger) Debugf(format string, v ...interface{}) {}

var l logger

func main() {
	sitelist := flag.String("sitelist", "", "File to read sites from, plain text")
	parallel := flag.Int("parallel", 2048, "Number of parallel requests")
	timeout := flag.Int("timeout", 5, "Timeout after seconds")
	// retries := flag.Int("retries", 2, "Number of retries")
	outputfilename := flag.String("output", "results.json", "Results output file name")

	flag.Parse()

	rawsites, err := ioutil.ReadFile(*sitelist)
	if err != nil {
		log.Println("Error reading sitelist file:", err)
		os.Exit(1)
	}

	lines := strings.Split(string(rawsites), "\n")

	var wg sync.WaitGroup
	queue := make(chan string, *parallel*2)
	results := make(chan result, *parallel*2)

	pb := progressbar.NewOptions(len(lines),
		progressbar.OptionEnableColorCodes(true),
		progressbar.OptionSetItsString("conn"),
		progressbar.OptionThrottle(time.Second*2),
		progressbar.OptionShowIts(),
		progressbar.OptionShowCount(),
	)
	// r := resty.New().
	// 	SetTimeout(time.Second * time.Duration(*timeout)).
	// 	SetRetryCount(*retries).
	// 	SetLogger(l).
	// 	SetCloseConnection(true).
	// 	R()

	for i := 0; i < *parallel; i++ {
		wg.Add(1)
		go func() {
			r := fasthttp.Client{}
			buffer := make([]byte, 16384)
			for site := range queue {
				code, body, err := r.GetTimeout(buffer, "https://"+site, time.Second*time.Duration(*timeout))
				if err == nil {
					results <- result{
						Site: site,
						// Headers: resp.Header(),
						Body: string(body),
						Code: code,
					}
					// resp.RawResponse.Body.Close()
				}
				pb.Add(1)
			}
			wg.Done()
		}()
	}

	resultsdata := make([]result, 0, len(lines))

	go func() {
		for result := range results {
			resultsdata = append(resultsdata, result)
		}
	}()

	for _, site := range lines {
		site = strings.Trim(site, "\r")
		queue <- site
	}

	close(queue)
	wg.Wait()
	pb.Finish()

	close(results)

	jd, err := json.Marshal(resultsdata)
	if err != nil {
		log.Println("Error encoding json:", err)
		os.Exit(1)
	}

	err = ioutil.WriteFile(*outputfilename, jd, 0660)
	if err != nil {
		log.Println("Error writing json:", err)
		os.Exit(1)
	}
}
