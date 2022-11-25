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

	for i := 0; i < runtime.NumCPU()/4; i++ {
		queueWG.Add(1)
		siteresults := make([]any, len(compiledRegexes))
		go func() {
			for file := range queue {
				var reader io.Reader
				raw, err := os.Open(file)
				if err != nil {
					log.Print("Error locating files to process:", err)
					continue
				}

				buf := bufio.NewReaderSize(raw, 16384*1024)

				reader = buf

				if strings.HasSuffix(strings.ToUpper(file), ".LZ4") {
					lzr := lz4.NewReader(buf)
					reader = lzr
				}
				data, err := io.ReadAll(reader)
				if err != nil {
					log.Print("Error reading data into memory:", err)
					continue
				}

				fmt.Printf("File %v loaded %v bytes\n", file, len(data))

				var matches int
				var sitecount int
				sitedata := data
				for {
					sitecount++
					end := bytes.Index(sitedata, splittoken)
					siteinfo := sitedata
					if end >= 0 {
						siteinfo = sitedata[:end]
					}

					localresults := 0
					for ri, re := range compiledRegexes {
						results := re.FindSubmatch(siteinfo)
						for i := 1; i < len(results); i++ {
							localresults++
							siteresults[ri] = results[i]
						}
					}
					if localresults == len(compiledRegexes) {
						matches++
						fmt.Fprintf(out, *format, siteresults...)
					}

					if end >= 0 {
						sitedata = sitedata[end+len(splittoken):]
					} else {
						break
					}
				}

				fmt.Printf("File %v has %v matches in %v sites\n", file, matches, sitecount)
				raw.Close()
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
