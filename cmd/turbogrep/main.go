package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/pierrec/lz4/v4"
	"github.com/schollz/progressbar/v3"
	"github.com/spf13/pflag"
)

func main() {
	input := pflag.String("input", "*.lz4", "Files to process")
	reg := pflag.StringArray("regexp", []string{"(?m)^\\*URL: (.+)$"}, "Regular expression to search for")
	format := pflag.String("format", "%s\n", "Output format for matches")
	output := pflag.String("output", "", "Output data to file")
	requiredmatches := pflag.Int("requiredmatches", -1, "Number of required matches, -1 means require all")
	pflag.Parse()

	files, err := filepath.Glob(*input)
	if err != nil {
		log.Print("Error locating files to process:", err)
		os.Exit(1)
	}

	var out io.Writer
	out = os.Stdout
	if *output != "" {
		outfile, err := os.Create(*output)
		if err != nil {
			log.Println("Error creating output file:", err)
			os.Exit(1)
		}
		defer outfile.Close()
		out = bufio.NewWriterSize(outfile, 4096*1024)
	}

	splittoken := []byte("+++++\n")

	type compiledRegexp struct {
		re       *regexp.Regexp
		subcount int
	}
	var totalsubcount int

	compiledRegexes := make([]compiledRegexp, len(*reg))
	var headers []any
	for i, exp := range *reg {
		re, err := regexp.Compile(exp)
		if err != nil {
			log.Printf("Error compiling regular expression %v: %v\n", exp, err)
			os.Exit(1)
		}
		for i, name := range re.SubexpNames() {
			if i > 0 { // Skip the first
				headers = append(headers, name)
			}
		}
		compiledRegexes[i] = compiledRegexp{
			re:       re,
			subcount: re.NumSubexp(),
		}
		totalsubcount += re.NumSubexp()
	}
	// CSV header
	fmt.Fprintf(out, *format, headers...)

	enoughmatches := *requiredmatches
	if enoughmatches == -1 {
		enoughmatches = totalsubcount
	}

	largestfile := -1
	pb := progressbar.NewOptions(
		len(files)*100000,
		progressbar.OptionFullWidth(),
		progressbar.OptionShowCount(),
		progressbar.OptionSetItsString("records"),
		progressbar.OptionSetElapsedTime(true),
		progressbar.OptionShowIts(),
		progressbar.OptionThrottle(time.Second*5),
	)

	queue := make(chan string, runtime.NumCPU())
	var queueWG sync.WaitGroup

	for i := 0; i < runtime.NumCPU(); i++ {
		queueWG.Add(1)

		localresults := make([]any, totalsubcount)

		buffer := make([]byte, 32000000)
		go func() {
			for file := range queue {
				var reader io.Reader
				raw, err := os.Open(file)
				if err != nil {
					log.Print("Error locating files to process:", err)
					continue
				}

				buf := bufio.NewReaderSize(raw, len(buffer))

				reader = buf

				if strings.HasSuffix(strings.ToUpper(file), ".LZ4") {
					lzr := lz4.NewReader(buf)
					reader = lzr
				}

				var matches int
				var records int

				// fmt.Printf("File %v loaded %v bytes\n", file, len(chunk))

				scan := bufio.NewScanner(reader)
				scan.Buffer(buffer, len(buffer))
				scan.Split(func(data []byte, atEOF bool) (advance int, token []byte, err error) {
					// No idea how this makes sense
					if atEOF && len(data) == 0 {
						return 0, nil, nil
					}
					// Delimited record
					end := bytes.Index(data, splittoken)
					if end >= 0 {
						return end + len(splittoken), data[:end], nil // We got one
					}
					// Final record
					if atEOF {
						return 0, data, bufio.ErrFinalToken
					}
					// Please read more data
					return 0, nil, nil
				})

				for scan.Scan() {
					record := scan.Bytes()

					pb.Add(1)
					records++

					localmatches := 0

					var resultoffset int
					for _, cr := range compiledRegexes {
						results := cr.re.FindSubmatch(record)
						if results != nil {
							localmatches++
							for i := 0; i < cr.subcount; i++ {
								localresults[resultoffset+i] = results[i+1]
							}
						} else {
							for i := 0; i < cr.subcount; i++ {
								localresults[resultoffset+i] = ""
							}
						}
						resultoffset += cr.subcount
					}
					if localmatches >= enoughmatches {
						matches++
						fmt.Fprintf(out, *format, localresults...)
					}
				}

				raw.Close()

				if records > largestfile {
					largestfile = records
					pb.ChangeMax(largestfile * len(files))
				}
				// fmt.Printf("File %v has %v matches in %v sites\n", file, matches, records)
			}
			queueWG.Done()
		}()
	}

	for _, file := range files {
		queue <- file
	}
	close(queue)
	queueWG.Wait()

	pb.Finish()
}
