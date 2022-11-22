package main

import (
	"encoding/json"
	"flag"
	"io/ioutil"
	"log"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/schollz/progressbar/v3"
	"github.com/valyala/fasthttp"
)

var l logger

func main() {
	sitelist := flag.String("sitelist", "", "File to read sites from, plain text")
	parallel := flag.Int("parallel", runtime.NumCPU()*64, "Number of parallel requests")
	timeout := flag.Int("timeout", 15, "Timeout after seconds")
	// retries := flag.Int("retries", 2, "Number of retries")
	outputfilename := flag.String("output", "results.json", "Results output file name")
	showerrors := flag.Bool("showerrors", false, "Show errors")

	flag.Parse()

	rawsites, err := ioutil.ReadFile(*sitelist)
	if err != nil {
		log.Println("Error reading sitelist file:", err)
		os.Exit(1)
	}

	lines := strings.Split(string(rawsites), "\n")

	var wg sync.WaitGroup
	queue := make(chan string, *parallel*2)
	results := make(chan Result, *parallel*2)

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
			r := fasthttp.Client{
				NoDefaultUserAgentHeader: true,
				MaxConnWaitTimeout:       time.Second * 240,
				ReadBufferSize:           128 * 1024,
			}
			buffer := make([]byte, 16384)
			for site := range queue {
				code, body, err := r.GetTimeout(buffer, "https://"+site, time.Second*time.Duration(*timeout))
				if err == nil {
					results <- Result{
						Site: site,
						// Headers: resp.Header(),
						Body: string(body),
						Code: code,
					}

					r.CloseIdleConnections()
					// resp.RawResponse.Body.Close()
				} else {
					if *showerrors {
						log.Println("Connecting to", site, "error:", err)
					}
				}
				pb.Add(1)
			}
			wg.Done()
		}()
	}

	output, err := os.Create(*outputfilename)
	if err != nil {
		log.Println("Error creating file:", err)
		os.Exit(1)
	}

	output.WriteString("[\n")
	go func() {
		for result := range results {
			jd, _ := json.MarshalIndent(result, "  ", "  ")
			_, err := output.Write(jd)
			if err != nil {
				log.Print("Error writing to output file:", err)
				os.Exit(1)
			}
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

	output.WriteString("]\n")
	output.Close()
}
