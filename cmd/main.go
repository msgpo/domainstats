/*
This program takes a list of domains and queries each against the OpenDNS
Investigate API, optionally outputting a CSV file.

Because querying every endpoint can be very time consuming, this program uses
a TOML file to configure which information should be queried.
*/
package main

import (
	"bufio"
	"encoding/csv"
	"flag"
	"fmt"
	"log"
	"os"
	"runtime"
	"strconv"
	"sync"

	"github.com/davecgh/go-spew/spew"
	"github.com/dead10ck/domainstats"
	"github.com/dead10ck/goinvestigate"
)

type opt struct {
	verbose       bool
	outFile       string
	configPath    string
	maxGoroutines int
}

var (
	opts              opt
	defaultConfigPath string = os.Getenv("HOME") + "/.domainstats/config.toml"
)

const (
	DEFAULT_MAX_GOROUTINES = 10
)

func init() {
	flag.IntVar(&opts.maxGoroutines, "m", DEFAULT_MAX_GOROUTINES,
		"Maximum number of goroutines to use for parallel HTTP requests")
	flag.BoolVar(&opts.verbose, "v", false, "Print out verbose log messages.")
	flag.StringVar(&opts.outFile, "out", "", "Output matching IPs to the given file")
	flag.StringVar(&opts.configPath, "c", defaultConfigPath, "The config file to use")

	runtime.GOMAXPROCS(runtime.NumCPU())
}

func main() {
	flag.Parse()
	config, err := domainstats.NewConfig(opts.configPath)
	if err != nil {
		log.Fatal(err)
	}
	//spew.Dump(config)
	var outWriter *csv.Writer
	var header []string
	domains := readDomainsFrom(flag.Arg(flag.NArg() - 1))
	inv := goinvestigate.New(config.APIKey)

	if opts.verbose {
		inv.SetVerbose(true)
	}

	//header = config.DeriveHeader()
	//fmt.Printf("header: %v\n", header)

	if opts.outFile != "" {
		outFile, err := os.Create(opts.outFile)
		if err != nil {
			log.Fatal(err)
		}
		outWriter = csv.NewWriter(outFile)
		header = config.DeriveHeader()
		outWriter.Write(header)
		defer func() {
			outWriter.Flush()
			outFile.Close()
		}()
	}

	inChan := make(chan string, len(domains))
	go func() {
		for _, d := range domains {
			inChan <- d
		}
		close(inChan)
	}()

	outChan := getInfo(config, inv, inChan)
	//getInfo(config, inv, inChan)

	numProcessed := 0

	for respRow := range outChan {
		numProcessed++
		fmt.Printf("\r%120s", " ")
		fmt.Printf("\r%d/%d: %v", numProcessed, len(domains), respRow)
		if outWriter != nil {
			outWriter.Write(respRow)
		}
	}
	fmt.Println()
}

func floatToStr(v interface{}) string {
	switch val := v.(type) {
	case float64:
		return strconv.FormatFloat(val, 'f', -1, 64)
	default:
		return ""
	}
}

// The goroutine which does the HTTP queries
func query(qChan <-chan *domainstats.DomainQueryMessage) {
	for m := range qChan {
		m.RespChan <- m.Q.Query()
	}
}

func process(inv *goinvestigate.Investigate, config *domainstats.Config,
	domainChan <-chan string, qChan chan<- *domainstats.DomainQueryMessage, wg *sync.WaitGroup) {
	respChan := make(chan domainstats.DomainQueryResponse, 10)
	for domain := range domainChan {

		// generate the list of queries to make for each domain
		queries := config.DeriveMessages(inv, domain, respChan)

		// send each query on the query channel for the query goroutines
		// to receive
		for _, q := range queries {
			qChan <- q
		}

		// receive once for each query that was sent
		for _, q := range queries {
			qmResp := <-q.RespChan
			if qmResp.Err != nil {
				inv.Logf("error during query for %v: %v", domain, qmResp.Err)
				continue
			}
			row, err := config.ExtractCSVSubRow(qmResp.Resp)
			if err != nil {
				inv.Logf("error extracting CSV sub row: %v", err)
				continue
			}
			spew.Dump(row)
		}

		//outChan <- ...
	}
	wg.Done()
}

func getInfo(config *domainstats.Config, inv *goinvestigate.Investigate, domainChan <-chan string) <-chan []string {
	outChan := make(chan []string, 100)
	qChan := make(chan *domainstats.DomainQueryMessage)
	wg := new(sync.WaitGroup)
	//fns := config.DeriveFuncCalls(inv)
	//fmt.Printf("fns: %v\n", fns)

	// launch the query goroutines
	for i := 0; i < opts.maxGoroutines; i++ {
		go query(qChan)
	}

	// launch the processor goroutines
	for i := 0; i < opts.maxGoroutines; i++ {
		wg.Add(1)
		go process(inv, config, domainChan, qChan, wg)
	}

	// launch a goroutine which closes the output channel when the processor
	// goroutines are finished
	go func() {
		wg.Wait()
		close(outChan)
	}()

	return outChan
}

func readDomainsFrom(fName string) (domains []string) {
	file, err := os.Open(fName)

	if err != nil {
		log.Fatalf("\nError opening domain list %s: %v\n", fName, err)
	}

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		domains = append(domains, scanner.Text())
	}
	return domains
}