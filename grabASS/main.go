package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"crypto/tls"
	"net/http"
	_ "net/http/pprof"
	"net/url"

	"github.com/OneOfOne/xxhash"
	"github.com/pierrec/lz4/v4"
	"github.com/schollz/progressbar/v3"
	"github.com/spf13/pflag"
	"github.com/valyala/fasthttp"
)

var l logger

func main() {
	// Grabbing data
	sitelist := pflag.String("sitelist", "", "File to read sites from, plain text")

	urlpaths := pflag.StringSlice("urlpath", []string{"/"}, "Path to grab")
	storecodes := pflag.IntSlice("storecodes", nil, "Return codes to store data from (default blank, means save all)")
	parallel := pflag.Int("parallel", runtime.NumCPU()*32, "Number of parallel requests")
	timeout := pflag.Int("timeout", 15, "Timeout after seconds")
	retries := pflag.Int("retries", 3, "Number of retries")
	useragent := pflag.String("useragent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/107.0.0.0 Safari/537.36 Edg/107.0.1418.56", "User agent to send to server")
	showerrors := pflag.Bool("showerrors", false, "Show errors")

	// Saving data
	outputfolder := pflag.String("outputfolder", "", "Results output folder name (if blank will use one file per site scanned)")
	format := pflag.String("format", "json", "Output format (plain, json)")
	compression := pflag.Bool("compress", false, "Store LZ4 compressed")
	recordsperfile := pflag.Int("perfile", 10000, "Number of records in each file")
	skipexisting := pflag.Bool("skipexisting", false, "Skip existing files, only works with perfile=1")

	// Debugging
	pprof := pflag.Bool("pprof", false, "Enable profiling")

	pflag.Parse()

	if *pprof {
		go func() {
			log.Println(http.ListenAndServe("localhost:6060", nil))
		}()
	}

	timeoutDuration := time.Second * time.Duration(*timeout)

	var codes map[int]struct{}
	if len(*storecodes) > 0 {
		codes = make(map[int]struct{})
		for _, code := range *storecodes {
			codes[code] = struct{}{}
		}
	}

	rawsites, err := os.ReadFile(*sitelist)
	if err != nil {
		log.Println("Error reading sitelist file:", err)
		os.Exit(1)
	}

	lines := strings.Split(string(rawsites), "\n")
	items := len(lines)

	var producerWG, writerWG sync.WaitGroup
	producerQueue := make(chan string, *parallel*2)

	encodedQueue := make(chan encoded, 1024)

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
			var err error
			secureclient := fasthttp.Client{
				NoDefaultUserAgentHeader: true,
				ReadBufferSize:           128 * 1024,
				WriteTimeout:             timeoutDuration,
				ReadTimeout:              timeoutDuration,
			}
			insecureclient := fasthttp.Client{
				TLSConfig:                &tls.Config{InsecureSkipVerify: true},
				NoDefaultUserAgentHeader: true,
				ReadBufferSize:           128 * 1024,
				WriteTimeout:             timeoutDuration,
				ReadTimeout:              timeoutDuration,
			}

			var req *fasthttp.Request
			var resp *fasthttp.Response

			for site := range producerQueue {
				client := &secureclient
				if *recordsperfile == 1 && *skipexisting {
					if _, err := os.Stat(generateFilename(*outputfolder, site, *recordsperfile, 0, items, *format, *compression)); err == nil {
						continue
					}
				}
				retriesleft := *retries
				var redirects int
				var code int
				var body, header, errstring string
				protocol := "https"

				urlpathindex := 0
				urlpath := (*urlpaths)[urlpathindex]

				var warnings []string
				var siteurl string
				host := site
			retryloop:
				for retriesleft > 0 {
					siteurl, err = url.JoinPath(protocol+"://"+host, urlpath)
					if err != nil {
						log.Printf("Problem creating URL from %v, %v, %v: %v\n", protocol, host, urlpath, err)
						break
					}

					if req != nil {
						fasthttp.ReleaseRequest(req)
						fasthttp.ReleaseResponse(resp)
					}
					req = fasthttp.AcquireRequest()
					req.SetConnectionClose()
					resp = fasthttp.AcquireResponse()
					resp.SetConnectionClose()

					req.Header.SetUserAgent(*useragent)
					req.SetRequestURI(siteurl)
					err = client.DoTimeout(req, resp, timeoutDuration)

					if err == nil {
						code = resp.Header.StatusCode()
						if fasthttp.StatusCodeIsRedirect(code) {
							redirects++
							if redirects > 3 {
								err = fasthttp.ErrTooManyRedirects
								break retryloop
							}

							newlocation := resp.Header.Peek("Location")
							if len(newlocation) == 0 {
								err = fasthttp.ErrMissingLocation
								break retryloop
							}

							baseurl, err := url.Parse(siteurl)
							if err != nil {
								err = fmt.Errorf("error parsing base URL %v: %v", siteurl, err)
								break retryloop
							}

							relativeurl, err := url.Parse(string(newlocation))
							if err != nil {
								err = fmt.Errorf("error parsing redirect location %v: %v", newlocation, err)
								break retryloop
							}

							newurl := baseurl.ResolveReference(relativeurl)

							host = newurl.Host
							urlpath = newurl.Path
							protocol = newurl.Scheme
							continue // retry
						} else if code != 200 && urlpathindex+1 < len(*urlpaths) {
							urlpathindex++
							urlpath = (*urlpaths)[urlpathindex]

							continue // retry
						}

						body = string(resp.Body())
						header = resp.Header.String()
						errstring = ""

						break retryloop
					}

					code = 0
					header = ""
					body = ""

					client.CloseIdleConnections()

					if err == fasthttp.ErrNoFreeConns {
						time.Sleep(time.Second)
						continue // try again, but it doesn't cost a retry
					} else if _, ok := err.(*net.DNSError); ok {
						if !strings.HasPrefix(host, "www.") {
							host = "www." + host
							warnings = append(warnings, "prefix_www")
							continue // loop without using a retry
						}
						// Just give up
						break retryloop
					} else if strings.Contains(err.Error(), "tls: failed to verify certificate: x509: certificate is valid for") {
						// Ignore bad certs
						warnings = append(warnings, "tls_wrong_host")
						client = &insecureclient
					} else if strings.Contains(err.Error(), "tls: failed to verify certificate: x509: certificate signed by unknown authority") {
						warnings = append(warnings, "tls_unknown_authority")
						client = &insecureclient
					} else if strings.Contains(err.Error(), "tls: failed to verify certificate: x509: certificate has expired or is not yet valid") {
						warnings = append(warnings, "tls_expired_cert")
						client = &insecureclient
					} else if err.Error() == "remote error: tls: internal error" {
						warnings = append(warnings, "unencrypted_http_failback")
						protocol = "http"
					} else if strings.Contains(err.Error(), "connectex: No connection could be made because the target machine actively refused it") {
						// Give up
						warnings = append(warnings, "connection_refused")
						break retryloop
					} else if err == fasthttp.ErrTooManyRedirects {
						// Give up
						break retryloop
					} else if err.Error() == "the server closed connection before returning the first response byte. Make sure the server returns 'Connection: close' response header before closing the connection" {

					} else {
						// other errors
					}

					time.Sleep(time.Second)

					if *showerrors {
						log.Println("Connecting to", siteurl, "error:", err.Error())
					}
					retriesleft--
				}

				// Check if we should save this or not
				if codes != nil {
					if _, found := codes[code]; !found {
						continue
					}
				}

				if err != nil {
					errstring = err.Error()
				}

				result := Result{
					Site:     site,
					URL:      siteurl,
					Header:   header,
					Body:     body,
					Code:     code,
					Error:    errstring,
					Warnings: warnings,
				}

				var jd []byte
				switch *format {
				case "json":
					jd = generateJSON(result)
				case "plain":
					jd = generatePlain(result)
				default:
					log.Println("Unknown format", *format)
					os.Exit(1)
				}
				encodedQueue <- encoded{
					name: result.Site,
					data: jd,
				}
			}
			producerWG.Done()
		}()
	}

	writerWG.Add(1)
	go func() {
		var written, fileno int
		var filename string
		var file *os.File
		var lz *lz4.Writer
		var writer io.Writer
		for encoded := range encodedQueue {
			if file == nil {
				filename = generateFilename(*outputfolder, encoded.name, *recordsperfile, fileno, items, *format, *compression)
				f, err := os.Create(filename)
				if err != nil {
					log.Printf("Error creating output file %v: %v", filename, err)
					os.Exit(1)
				}
				file = f
				writer = f
				if *compression {
					lz = lz4.NewWriter(file)
					lz.Apply(
						lz4.CompressionLevelOption(lz4.Level9),
						lz4.ConcurrencyOption(-1),
						lz4.BlockSizeOption(lz4.Block4Mb),
					)
					writer = lz
				}
			}

			_, err := writer.Write(encoded.data)
			if err != nil {
				log.Printf("Error writing output file %v: %v", filename, err)
			}
			written++
			if written == *recordsperfile {
				if *compression {
					lz.Close()
				}
				file.Close()
				file = nil
				written = 0
				fileno++
			}
		}
		writerWG.Done()
	}()

	for _, site := range lines {
		site = strings.Trim(site, "\r")
		producerQueue <- site
		pb.Add(1)
	}

	close(producerQueue)
	producerWG.Wait()
	pb.Finish()

	close(encodedQueue)
	writerWG.Wait()
}

func generateFilename(folder, name string, itemsperfile, currentfile, totalitems int, format string, compression bool) string {
	filename := name
	if totalitems/itemsperfile > 1024 {
		foldercount := uint64(totalitems / itemsperfile / 1024)
		hash := uint64(xxhash.Checksum64S([]byte(name), 0)) % foldercount
		subfoldername := fmt.Sprintf("%03x", hash)
		folder = filepath.Join(folder, subfoldername)

		filename = filepath.Join(folder, filename)
	} else {
		filename = filepath.Join(folder, filename)
	}

	if folder != "" {
		os.MkdirAll(folder, 0600)
	} else {
		filename += fmt.Sprintf("-%09d", currentfile)
	}

	filename += "." + format
	if compression {
		filename += ".lz4"
	}
	return filename
}

func generateJSON(data Result) []byte {
	result, _ := json.Marshal(data)
	return result
}

func generatePlain(data Result) []byte {
	var buffer bytes.Buffer
	buffer.Grow(len(data.Header) + len(data.Body) + 128)

	buffer.WriteString("*Site: ")
	buffer.WriteString(data.Site)
	buffer.WriteString("\n")

	buffer.WriteString("*URL: ")
	buffer.WriteString(data.URL)
	buffer.WriteString("\n")

	if len(data.Warnings) > 0 {
		buffer.WriteString("*Warnings: ")
		buffer.WriteString(strings.Join(data.Warnings, ", "))
		buffer.WriteString("\n")
	}

	if data.Error != "" {
		buffer.WriteString("*Error: ")
		buffer.WriteString(data.Error)
		buffer.WriteString("\n")
	} else {
		buffer.WriteString(fmt.Sprintf("*Resultcode: %v\n", data.Code))
		buffer.WriteString("-----\n")
		buffer.WriteString(data.Header)
		buffer.WriteString("=====\n")
		buffer.WriteString(data.Body)
		buffer.WriteString("\n")
	}
	buffer.WriteString("+++++\n")
	return buffer.Bytes()
}
