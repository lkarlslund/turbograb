package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
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
	"github.com/pierrec/lz4/v4"
	"github.com/schollz/progressbar/v3"
	"github.com/valyala/fasthttp"
)

var l logger

func main() {
	// Grabbing data
	sitelist := flag.String("sitelist", "", "File to read sites from, plain text")
	parallel := flag.Int("parallel", runtime.NumCPU()*32, "Number of parallel requests")
	timeout := flag.Int("timeout", 15, "Timeout after seconds")
	retries := flag.Int("retries", 2, "Number of retries")
	useragent := flag.String("useragent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/107.0.0.0 Safari/537.36 Edg/107.0.1418.56", "User agent to send to server")
	showerrors := flag.Bool("showerrors", false, "Show errors")
	// Saving data
	outputfolder := flag.String("outputfolder", "", "Results output folder name (if blank will use one file per site scanned)")
	format := flag.String("format", "json", "Output format")
	compression := flag.Bool("compress", false, "Store LZ4 compressed")
	recordsperfile := flag.Int("perfile", 10000, "Number of records in each file")
	// Debugging
	pprof := flag.Bool("pprof", false, "Enable profiling")

	flag.Parse()

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
			client := fasthttp.Client{
				NoDefaultUserAgentHeader: true,
				ReadBufferSize:           128 * 1024,
			}

			var req *fasthttp.Request
			var resp *fasthttp.Response

			for site := range producerQueue {
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
				pb.Add(1)
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

	buffer.WriteString("*URL: ")
	buffer.WriteString(data.Site)
	buffer.WriteString("\n")

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
