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
	"runtime/pprof"
	"slices"
	"strings"
	"sync"
	"time"

	"crypto/tls"
	"crypto/x509"
	"net/http"
	_ "net/http/pprof"
	"net/url"

	"github.com/OneOfOne/xxhash"
	"github.com/grantae/certinfo"
	"github.com/lkarlslund/turbograb"
	"github.com/pierrec/lz4/v4"
	"github.com/schollz/progressbar/v3"
	"github.com/spf13/pflag"
	"github.com/valyala/fasthttp"
)

func main() {
	// Grabbing data
	sitelist := pflag.String("sitelist", "", "File to read sites from (plain text) or comma separated list")

	urlpaths := pflag.StringSlice("urlpath", []string{"/"}, "Path to grab")
	storecodes := pflag.IntSlice("storecodes", nil, "Return codes to store data from (default blank, means save all)")
	parallel := pflag.Int("parallel", runtime.NumCPU()*32, "Number of parallel requests")
	timeout := pflag.Int("timeout", 15, "Timeout after seconds")
	maxretries := pflag.Int("retries", 10, "Max number of retries")
	maxredirects := pflag.Int("redirects", 5, "Max number of redirects")
	maxresponsesize := pflag.Int("maxresponsesize", 32*1024*1024, "Max response size in bytes")
	useragent := pflag.String("useragent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/107.0.0.0 Safari/537.36 Edg/107.0.1418.56", "User agent to send to server")
	showerrors := pflag.Bool("showerrors", false, "Show errors")

	// Saving data
	outputfolder := pflag.String("outputfolder", "", "Results output folder name (if blank will use one file per site scanned)")
	format := pflag.String("format", "json", "Output format (txt, json)")
	compression := pflag.Bool("compress", false, "Store LZ4 compressed")
	recordsperfile := pflag.Int("perfile", 10000, "Number of records in each file")
	buckets := pflag.Int("buckets", 4096, "Number of buckets to place files in")
	skipnewerthan := pflag.Int("skipnewerthan", 7*1440, "Skip existing files that are newer than N minutes, only works with perfile=1")

	// Debugging
	pprofenable := pflag.Bool("pprof", false, "Enable profiling")

	pflag.Parse()

	if *pprofenable {
		go func() {
			log.Println(http.ListenAndServe("localhost:6060", nil))
		}()
	}

	go func() {
		var ms runtime.MemStats
		for {
			time.Sleep(time.Millisecond * 50)
			runtime.ReadMemStats(&ms)
			if ms.HeapAlloc > 4*1024*1024*1024 {
				log.Printf("FATAL MEMORY HOG ERROR: %d MB", ms.HeapAlloc/1024/1024)
				f, err := os.Create("mem_profile.prof")
				if err != nil {
					panic(err)
				}

				err = pprof.WriteHeapProfile(f)
				if err != nil {
					panic(err)
				}
				f.Close()

				panic("FATAL MEMORY HOG ERROR")
			}
		}
	}()

	timeoutDuration := time.Second * time.Duration(*timeout)

	var codes map[int]struct{}
	if len(*storecodes) > 0 {
		codes = make(map[int]struct{})
		for _, code := range *storecodes {
			codes[code] = struct{}{}
		}
	}

	var sites []string
	if !strings.Contains(*sitelist, ",") {
		rawsites, err := os.ReadFile(*sitelist)
		if err != nil {
			if !strings.Contains(*sitelist, "/") || !strings.HasSuffix(*sitelist, "\\") {
				log.Printf("Sitelist file %v not found, assuming it's a hostname", *sitelist)
				sites = []string{*sitelist}
			} else {
				log.Println("Error reading sitelist file:", err)
				os.Exit(1)
			}
		} else {
			sites = strings.Split(string(rawsites), "\n")
		}
	} else if !strings.Contains(*sitelist, "/") || !strings.HasSuffix(*sitelist, "\\") {
		sites = strings.Split(*sitelist, ",")
	} else {
		log.Println("Sitelist parameter is not a file or a list of hostnames")
		os.Exit(1)
	}

	var producerWG, writerWG sync.WaitGroup
	producerQueue := make(chan string, *parallel*4)

	encodedQueue := make(chan turbograb.Encoded, runtime.NumCPU()*4)

	pb := progressbar.NewOptions(len(sites),
		progressbar.OptionEnableColorCodes(true),
		progressbar.OptionSetItsString("conn"),
		progressbar.OptionThrottle(time.Second*2),
		progressbar.OptionFullWidth(),
		progressbar.OptionShowIts(),
		progressbar.OptionShowCount(),
	)

	fasthttp.SetBodySizePoolLimit(65536, 65536)

	// Producers
	for i := 0; i < *parallel; i++ {
		producerWG.Add(1)
		go func() {
			var certinfo []*x509.Certificate
			securetls := &tls.Config{
				VerifyPeerCertificate: storecertinfo(&certinfo),
			}
			insecuretls := &tls.Config{
				InsecureSkipVerify:    true,
				VerifyPeerCertificate: storecertinfo(&certinfo),
			}

			var hostclient *fasthttp.HostClient
			var req *fasthttp.Request
			var resp *fasthttp.Response
			var requestshandled int

			for site := range producerQueue {
				var siteerr error
				certinfo = nil

				if *recordsperfile == 1 && *skipnewerthan > 0 {
					if stat, err := os.Stat(generateFilename(*outputfolder, site, *recordsperfile, *buckets, *format, *compression)); err == nil {
						if time.Since(stat.ModTime()) < time.Minute*time.Duration(*skipnewerthan) {
							continue
						}
					}
				}

				retriesleft := *maxretries
				redirectsleft := *maxredirects
				var code int
				var ipaddress, body, header, errstring string

				if hostclient != nil && hostclient.ConnsCount() > 0 {
					hostclient.CloseIdleConnections()
					time.Sleep(time.Second * 2) // Wait for background cleanup goroutine to finish, sic
				}

				protocol := "https"
				hostclient = &fasthttp.HostClient{
					IsTLS:                    true,
					TLSConfig:                securetls,
					NoDefaultUserAgentHeader: true,
					MaxResponseBodySize:      *maxresponsesize,
					WriteTimeout:             timeoutDuration,
					ReadTimeout:              timeoutDuration,
					MaxConns:                 1,
					MaxIdleConnDuration:      time.Millisecond * 1100, // A tiny amount more than the sleep interval
				}
				urlpathindex := 0
				urlpath := (*urlpaths)[urlpathindex]

				closerequest := true
				justnotcloserequest := false

				var warnings []string
				var siteurl string
				host := site
			retryloop:
				for retriesleft > 0 {
					if protocol == "https" {
						hostclient.IsTLS = true
						hostclient.Addr = host + ":443"
					} else if protocol == "http" {
						hostclient.IsTLS = false
						hostclient.Addr = host + ":80"
					}

					if !strings.HasPrefix(urlpath, "/") {
						urlpath = "/" + urlpath
					}

					uri := fasthttp.AcquireURI()
					siteerr = uri.Parse(nil, []byte(protocol+"://"+host+urlpath))
					if siteerr != nil {
						break retryloop
					}

					if req != nil {
						fasthttp.ReleaseRequest(req)
						fasthttp.ReleaseResponse(resp)
					}

					req = fasthttp.AcquireRequest()
					resp = fasthttp.AcquireResponse()

					if closerequest {
						req.SetConnectionClose()
						resp.SetConnectionClose()
					}

					req.Header.SetUserAgent(*useragent)
					req.SetURI(uri)
					fasthttp.ReleaseURI(uri)

					siteerr = hostclient.DoTimeout(req, resp, timeoutDuration)

					requestshandled++

					ipaddress = resp.RemoteAddr().String()

					if siteerr == nil {
						code = resp.Header.StatusCode()

						if code == 200 || code == 206 {
							body = string(resp.Body())
							header = resp.Header.String()
							errstring = ""
							break retryloop
						}

						if fasthttp.StatusCodeIsRedirect(code) {
							warnings = append(warnings, "redirect")

							redirectsleft--
							if redirectsleft == 0 {
								siteerr = fasthttp.ErrTooManyRedirects
								break retryloop
							}

							newlocation := resp.Header.Peek("Location")
							if len(newlocation) == 0 {
								siteerr = fasthttp.ErrMissingLocation
								break retryloop
							}

							var baseurl *url.URL
							baseurl, siteerr = url.Parse(siteurl)
							if siteerr != nil {
								siteerr = fmt.Errorf("error parsing base URL %v: %v", siteurl, siteerr)
								break retryloop
							}

							var relativeurl *url.URL
							relativeurl, siteerr = url.Parse(string(newlocation))
							if siteerr != nil {
								siteerr = fmt.Errorf("error parsing redirect location %v: %v", newlocation, siteerr)
								break retryloop
							}

							newurl := baseurl.ResolveReference(relativeurl)

							var newsiteurl string
							newsiteurl, siteerr = url.JoinPath(newurl.Scheme+"://"+newurl.Host, newurl.Path)
							if siteerr != nil {
								siteerr = fmt.Errorf("error creating new site URL from %v: %v", newurl, siteerr)
								break retryloop
							}

							if strings.EqualFold(newsiteurl, siteurl) {
								if !justnotcloserequest {
									warnings = append(warnings, "redirect_to_self")
									if closerequest {
										closerequest = false
										justnotcloserequest = true
									} else {
										siteerr = fasthttp.ErrTooManyRedirects
										break retryloop
									}
								} else {
									justnotcloserequest = false
								}
							}

							if host != newurl.Host {
								warnings = append(warnings, "redirect_to_other_host")
							}

							host = newurl.Host

							if urlpath != newurl.Path {
								warnings = append(warnings, "redirect_to_other_path")
							}

							urlpath = newurl.Path

							if protocol == "https" && newurl.Scheme == "http" {
								warnings = append(warnings, "https_to_http_redirect")
							}

							protocol = newurl.Scheme
							continue // retry
						} else if urlpathindex+1 < len(*urlpaths) {
							// Try another default URL
							urlpathindex++
							urlpath = (*urlpaths)[urlpathindex]

							continue // retry
						}
					} else {
						// There was an error
						code = 0
						header = ""
						body = ""

						if siteerr == fasthttp.ErrBodyTooLarge {
							siteerr = fmt.Errorf("%v (%v bytes)", siteerr.Error(), resp.Header.ContentLength())
							break retryloop
						} else if siteerr == fasthttp.ErrNoFreeConns {
							time.Sleep(time.Second)
							continue // try again, but it doesn't cost a retry
						} else if _, ok := siteerr.(*net.DNSError); ok {
							if !strings.HasPrefix(host, "www.") {
								host = "www." + host
								warnings = append(warnings, "prefix_www")
								continue // loop without using a retry
							}
							// Just give up
							break retryloop
						} else if strings.Contains(siteerr.Error(), "tls: failed to verify certificate: x509: certificate is valid for") {
							// Ignore bad certs
							warnings = append(warnings, "tls_wrong_host")
							hostclient.TLSConfig = insecuretls
						} else if strings.Contains(siteerr.Error(), "tls: failed to verify certificate: x509: certificate signed by unknown authority") {
							warnings = append(warnings, "tls_unknown_authority")
							hostclient.TLSConfig = insecuretls
						} else if strings.Contains(siteerr.Error(), "tls: failed to verify certificate: x509: certificate has expired or is not yet valid") {
							warnings = append(warnings, "tls_expired_cert")
							hostclient.TLSConfig = insecuretls
						} else if siteerr.Error() == "remote error: tls: internal error" {
							warnings = append(warnings, "unencrypted_http_failback")
							protocol = "http"
						} else if strings.Contains(siteerr.Error(), "connectex: No connection could be made because the target machine actively refused it") {
							// Give up
							warnings = append(warnings, "connection_refused")
							break retryloop
						} else if siteerr == fasthttp.ErrTooManyRedirects {
							// Give up
							break retryloop
						} else if siteerr.Error() == "the server closed connection before returning the first response byte. Make sure the server returns 'Connection: close' response header before closing the connection" {

						} else {
							// other errors
							warnings = append(warnings, strings.ReplaceAll(siteerr.Error(), " ", "_"))
						}
					}

					time.Sleep(time.Second)

					if *showerrors {
						log.Println("Connecting to", siteurl, "error:", siteerr.Error())
					}
					retriesleft--
				}

				// Check if we should save this or not
				if codes != nil {
					if _, found := codes[code]; !found {
						continue
					}
				}

				if siteerr != nil {
					errstring = siteerr.Error()
				}

				// Unique warnings only
				slices.Sort(warnings)
				warnings = slices.Compact(warnings)

				// Ship it!
				result := turbograb.Result{
					Site:         site,
					URL:          siteurl,
					Certificates: certinfo,
					Header:       header,
					Body:         body,
					IPaddress:    ipaddress,
					Code:         code,
					Error:        errstring,
					Warnings:     warnings,
				}

				var jd []byte
				switch *format {
				case "json":
					jd = generateJSON(result)
				case "txt":
					jd = generateTXT(result)
				default:
					log.Println("Unknown format", *format)
					os.Exit(1)
				}
				encodedQueue <- turbograb.Encoded{
					Site: result.Site,
					Data: jd,
				}
			}
			producerWG.Done()
		}()
	}

	maxwriters := 1
	if *recordsperfile == 1 {
		maxwriters = runtime.NumCPU()
	}
	for i := 0; i < maxwriters; i++ {
		writerWG.Add(1)
		go func() {
			var written, fileno int
			var filename string
			var file *os.File
			var lz *lz4.Writer
			var writer io.Writer
			for encoded := range encodedQueue {
				if file == nil {
					filename = generateFilename(*outputfolder, encoded.Site, *recordsperfile, *buckets, *format, *compression)
					f, err := os.Create(filename)
					if err != nil {
						log.Printf("Error creating output file %v: %v", filename, err)
						os.Exit(1)
					}
					file = f
					writer = f
					if *compression {
						lz = lz4.NewWriter(file)
						err = lz.Apply(
							lz4.CompressionLevelOption(lz4.Level9),
							lz4.ConcurrencyOption(-1),
							lz4.BlockSizeOption(lz4.Block4Mb),
						)
						if err != nil {
							log.Printf("Error creating lz4 writer: %v", err)
						}
						writer = lz
					}
				}

				_, err := writer.Write(encoded.Data)
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
	}

	for _, site := range sites {
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

func generateFilename(folder, name string, itemsperfile, buckets int, format string, compression bool) string {
	filename := name

	if buckets > 1 {
		hashbucket := uint64(xxhash.Checksum64S([]byte(name), 0)) % uint64(buckets)
		subfoldername := fmt.Sprintf("%04x", hashbucket)
		folder = filepath.Join(folder, subfoldername)

		filename = filepath.Join(folder, filename)
	} else {
		filename = filepath.Join(folder, filename)
	}

	if folder != "" {
		os.MkdirAll(folder, 0600)
	}

	filename += "." + format
	if compression {
		filename += ".lz4"
	}
	return filename
}

func generateJSON(data turbograb.Result) []byte {
	result, _ := json.Marshal(data)
	return result
}

func generateTXT(data turbograb.Result) []byte {
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
		buffer.WriteString("*IP: ")
		buffer.WriteString(data.IPaddress)
		buffer.WriteString("\n")
		buffer.WriteString(fmt.Sprintf("*Resultcode: %v\n", data.Code))
	}

	if len(data.Certificates) > 0 {
		for _, cert := range data.Certificates {
			info, err := certinfo.CertificateText(cert)
			if err != nil {
				continue
			}
			buffer.WriteString("*****\n")
			buffer.WriteString(info)
		}
	}

	if data.Error == "" {
		buffer.WriteString("-----\n")
		buffer.WriteString(data.Header)
		buffer.WriteString("=====\n")
		buffer.WriteString(data.Body)
		buffer.WriteString("\n")
	}
	buffer.WriteString("+++++\n")
	return buffer.Bytes()
}

func storecertinfo(store *[]*x509.Certificate) func(rawCerts [][]byte, verifiedChains [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, verifiedChains [][]*x509.Certificate) error {
		*store = make([]*x509.Certificate, 0, len(rawCerts))
		for _, rawCert := range rawCerts {
			cert, err := x509.ParseCertificate(rawCert)
			if err != nil {
				fmt.Printf("Error parsing certificate: %v\n", err)
				continue
			}
			*store = append(*store, cert)
		}
		return nil
	}
}
