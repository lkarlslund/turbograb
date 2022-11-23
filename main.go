package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"net/http"
	_ "net/http/pprof"

	"github.com/OneOfOne/xxhash"
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
	overwrite := flag.Bool("overwrite", true, "Overwrite existing files, set to false with many hosts to resume")
	useragent := flag.String("useragent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/107.0.0.0 Safari/537.36 Edg/107.0.1418.56", "User agent to send to server")
	showerrors := flag.Bool("showerrors", false, "Show errors")
	pprof := flag.Bool("pprof", false, "Enable profiling")

	flag.Parse()

	onefile := *outputfilename != "" && !strings.HasSuffix(*outputfilename, "\\") && !strings.HasSuffix(*outputfilename, "/")

	if *pprof {
		go func() {
			log.Println(http.ListenAndServe("localhost:6060", nil))
		}()
	}

	rawsites, err := ioutil.ReadFile(*sitelist)
	if err != nil {
		log.Println("Error reading sitelist file:", err)
		os.Exit(1)
	}

	lines := strings.Split(string(rawsites), "\n")
	items := len(lines)

	var producerWG, consumerWG, writerWG sync.WaitGroup
	producerQueue := make(chan string, *parallel*2)
	consumerQueue := make(chan Result, runtime.NumCPU()*2)

	type encoded struct {
		name string
		data []byte
	}
	encodedQueue := make(chan encoded, runtime.NumCPU()*2)

	pb := progressbar.NewOptions(len(lines),
		progressbar.OptionEnableColorCodes(true),
		progressbar.OptionSetItsString("conn"),
		progressbar.OptionThrottle(time.Second*2),
		progressbar.OptionShowIts(),
		progressbar.OptionShowCount(),
	)

	for i := 0; i < *parallel; i++ {
		producerWG.Add(1)
		go func() {
			client := fasthttp.Client{
				NoDefaultUserAgentHeader: true,
				ReadBufferSize:           128 * 1024,
			}

			var req *fasthttp.Request
			var resp *fasthttp.Response

			for site := range producerQueue {
				if !onefile && !*overwrite {
					if _, err := os.Stat(generateFilename(*outputfilename, site, items)); err == nil {
						// Exists, skip it (DOES NOT TAKE REDIRECTION INTO ACCOUNT!)
						pb.Add(1)
						continue
					}
				}

				retriesleft := *retries
				var redirects int
				var code int
				var body, header, errstring string
				protocol := "https"
				location := protocol + "://" + site
			retryloop:
				for retriesleft > 0 {
					if req != nil {
						fasthttp.ReleaseRequest(req)
						fasthttp.ReleaseResponse(resp)
					}
					req = fasthttp.AcquireRequest()
					resp = fasthttp.AcquireResponse()

					req.Header.SetUserAgent(*useragent)
					req.SetRequestURI(location)
					err := client.DoTimeout(req, resp, time.Second*time.Duration(*timeout))

					if err == nil {
						code = resp.Header.StatusCode()
						if fasthttp.StatusCodeIsRedirect(code) {
							redirects++
							if redirects > 3 {
								err = fasthttp.ErrTooManyRedirects
								break
							}

							newlocation := resp.Header.Peek("Location")
							if len(newlocation) == 0 {
								err = fasthttp.ErrMissingLocation
								break
							}

							u := fasthttp.AcquireURI()
							u.Update(location)
							u.UpdateBytes(newlocation)
							location = u.String()
							fasthttp.ReleaseURI(u)

							continue
						}

						body = string(resp.Body())
						header = resp.Header.String()

						errstring = ""
					} else {
						code = 0
						header = ""
						body = ""
						errstring = err.Error()
					}

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
				consumerQueue <- result
				pb.Add(1)
			}
			producerWG.Done()
		}()
	}

	for i := 0; i < runtime.NumCPU(); i++ {
		consumerWG.Add(1)
		go func() {
			for result := range consumerQueue {
				jd, _ := json.MarshalIndent(result, "", "  ")
				encodedQueue <- encoded{
					name: result.Site,
					data: jd,
				}
			}
			consumerWG.Done()
		}()
	}

	if onefile {
		output, err := os.Create(*outputfilename)
		if err != nil {
			log.Println("Error creating file:", err)
			os.Exit(1)
		}
		output.WriteString("[\n")
		writerWG.Add(1)
		go func() {
			for encoded := range encodedQueue {
				_, err := output.Write(encoded.data)
				if err != nil {
					log.Print("Error writing to output file:", err)
					os.Exit(1)
				}
			}
			output.WriteString("]\n")
			output.Close()
			writerWG.Done()
		}()
	} else {
		for i := 0; i < runtime.NumCPU(); i++ {
			writerWG.Add(1)
			go func() {
				for encoded := range encodedQueue {
					filename := generateFilename(*outputfilename, encoded.name, items)
					err := ioutil.WriteFile(filename, encoded.data, 0600)
					if err != nil {
						log.Printf("Error writing output file %v: %v", filename, err)
					}
				}
				writerWG.Done()
			}()
		}
	}

	for _, site := range lines {
		site = strings.Trim(site, "\r")
		producerQueue <- site
	}

	close(producerQueue)
	producerWG.Wait()
	pb.Finish()

	close(consumerQueue)
	consumerWG.Wait()

	close(encodedQueue)
	writerWG.Wait()
}

func generateFilename(prefix, name string, totalitems int) string {
	var folder string
	filename := name + ".json"
	if totalitems > 1024 {
		foldercount := uint64(totalitems / 1024)
		hash := uint64(xxhash.Checksum64S([]byte(name), 0)) % foldercount
		subfoldername := fmt.Sprintf("%08x", hash)
		folder = filepath.Join(prefix, subfoldername)
		filename = filepath.Join(folder, filename)
	} else {
		filename = filepath.Join(prefix, filename)
	}
	if folder != "" {
		os.MkdirAll(folder, 0600)
	}
	return filename
}
