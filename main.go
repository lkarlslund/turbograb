package main

import (
	"encoding/json"
	"flag"
	"io/ioutil"
	"log"
	"net"
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
	parallel := flag.Int("parallel", runtime.NumCPU()*32, "Number of parallel requests")
	timeout := flag.Int("timeout", 15, "Timeout after seconds")
	retries := flag.Int("retries", 2, "Number of retries")
	outputfilename := flag.String("output", "", "Results output file name (if blank will use one file per site scanned)")
	useragent := flag.String("useragent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/107.0.0.0 Safari/537.36 Edg/107.0.1418.56", "User agent to send to server")
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

	for i := 0; i < *parallel; i++ {
		wg.Add(1)
		go func() {
			client := fasthttp.Client{
				NoDefaultUserAgentHeader: true,
				ReadBufferSize:           128 * 1024,
			}

			for site := range queue {
				retriesleft := *retries
				var code int
				var body, header, errstring string
				protocol := "https"
			retryloop:
				for retriesleft > 0 {
					req := fasthttp.AcquireRequest()
					resp := fasthttp.AcquireResponse()
					defer fasthttp.ReleaseRequest(req)
					defer fasthttp.ReleaseResponse(resp)

					req.Header.SetUserAgent(*useragent)
					req.SetRequestURI(protocol + "://" + site)

					err := client.DoTimeout(req, resp, time.Second*time.Duration(*timeout))

					if err != nil {
						errstring = err.Error()
					} else {
						errstring = ""
					}

					body = string(resp.Body())
					header = resp.Header.String()
					code = resp.StatusCode()

					// code, body, err = r.GetTimeout(buffer, "https://"+site, time.Second*time.Duration(*timeout))
					client.CloseIdleConnections()

					switch e := err.(type) {
					case *net.DNSError:
						if !strings.HasPrefix(site, "www.") {
							site = "www." + site
							continue // loop without using a retry
						}
						// Just give up
						break retryloop
					case nil:
						break retryloop
					default:
						if e.Error() == "the server closed connection before returning the first response byte. Make sure the server returns 'Connection: close' response header before closing the connection" {

						} else {
							// other errors
						}
						if *showerrors {
							log.Println("Connecting to", site, "error:", e)
						}
					}
					retriesleft--
				}
				result := Result{
					Site:   site,
					Header: header,
					Body:   body,
					Code:   code,
					Error:  errstring,
				}
				results <- result
				pb.Add(1)
			}
			wg.Done()
		}()
	}

	if *outputfilename != "" {
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
			output.WriteString("]\n")
			output.Close()
		}()
	} else {
		go func() {
			for result := range results {
				filename := result.Site + ".json"
				jd, _ := json.MarshalIndent(result, "", "  ")
				err := ioutil.WriteFile(filename, jd, 0600)
				if err != nil {
					log.Printf("Error writing output file %v: %v", filename, err)
				}
			}
		}()
	}

	for _, site := range lines {
		site = strings.Trim(site, "\r")
		queue <- site
	}

	close(queue)
	wg.Wait()
	pb.Finish()

	close(results)

}
