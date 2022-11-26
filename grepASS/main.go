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

	"github.com/pierrec/lz4/v4"
	"github.com/spf13/pflag"
)

func main() {
	input := pflag.String("input", "*.lz4", "Files to process")
	reg := pflag.StringArray("regexp", []string{"(?m)^\\*URL: (.+)$"}, "Regular expression to search for")
	format := pflag.String("format", "%s\n", "Output format for matches")
	output := pflag.String("output", "", "Output data to file")
	pflag.Parse()

	var out io.Writer
	out = os.Stdout
	if *output != "" {
		outfile, err := os.Create(*output)
		if err != nil {
			log.Println("Error creating output file:", err)
			os.Exit(1)
		}
		out = bufio.NewWriterSize(outfile, 4096*1024)
	}

	splittoken := []byte("+++++\n")

	compiledRegexes := make([]*regexp.Regexp, len(*reg))
	for i, exp := range *reg {
		re, err := regexp.Compile(exp)
		if err != nil {
			log.Printf("Error compiling regular expression %v: %v\n", exp, err)
			os.Exit(1)
		}
		compiledRegexes[i] = re
	}

	queue := make(chan string, runtime.NumCPU())
	var queueWG sync.WaitGroup

	for i := 0; i < runtime.NumCPU(); i++ {
		queueWG.Add(1)
		siteresults := make([]any, len(compiledRegexes))
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

					records++

					localresults := 0
					for ri, re := range compiledRegexes {
						results := re.FindSubmatch(record)
						for i := 1; i < len(results); i++ {
							localresults++
							siteresults[ri] = results[i]
						}
					}
					if localresults == len(compiledRegexes) {
						matches++
						fmt.Fprintf(out, *format, siteresults...)
					}
				}

				raw.Close()
				fmt.Printf("File %v has %v matches in %v sites\n", file, matches, records)
			}
			queueWG.Done()
		}()
	}

	files, err := filepath.Glob(*input)
	if err != nil {
		log.Print("Error locating files to process:", err)
		os.Exit(1)
	}
	for _, file := range files {
		queue <- file
	}
	close(queue)
	queueWG.Wait()
}
