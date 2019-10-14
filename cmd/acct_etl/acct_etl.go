package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"log"
	"os"
	"time"

	c "github.com/tuck1s/sparkyPMTATracking"

	"github.com/go-redis/redis"
	"github.com/smartystreets/scanners/csv"
)

// Event information in redis will have this time-to-live
const ttl = time.Duration(time.Hour * 24 * 10)

// Scan input accounting records - required fields for enrichment must include type, header_x-sp-message-id.
// Delivery records are of type "d".
const typeField = "type"
const msgIDField = "header_x-sp-message-id"
const deliveryType = "d"

var requiredAcctFields = []string{
	typeField, msgIDField,
}
var optionalAcctFields = []string{
	"orig", "rcpt", "jobId", "dlvSourceIp", "vmtaPool",
}

// Acccounting header record sent at PowerMTA start, check for required and optional fields.
// Write these into persistent storage, so that we can decode "d" records in future, separate process invocations.
func storeHeaders(r []string, client *redis.Client) {
	log.Println("PMTA accounting headers from pipe ", r)
	hdrs := make(map[string]int)
	for _, f := range requiredAcctFields {
		fpos, found := c.PositionIn(r, f)
		if found {
			hdrs[f] = fpos
		} else {
			c.ConsoleAndLogFatal("Required field", f, "is not present in PMTA accounting headers")
		}
	}
	// Pick up positions of optional fields, for event enrichment
	for _, f := range optionalAcctFields {
		fpos, found := c.PositionIn(r, f)
		if found {
			hdrs[f] = fpos
		}
	}
	hdrsJSON, err := json.Marshal(hdrs)
	c.Check(err)
	_, err = client.Set(c.RedisAcctHeaders, hdrsJSON, 0).Result()
	c.Check(err)
	log.Println("Loaded", c.RedisAcctHeaders, "->", string(hdrsJSON), "into Redis")
}

// Store a single accounting event r into redis, based on previously seen header format
func storeEvent(r []string, client *redis.Client) {
	hdrsJ, err := client.Get(c.RedisAcctHeaders).Result()
	if err == redis.Nil {
		c.ConsoleAndLogFatal("Error: redis key", c.RedisAcctHeaders, "not found")
	}
	hdrs := make(map[string]int)
	err = json.Unmarshal([]byte(hdrsJ), &hdrs)
	c.Check(err)
	// read fields from r into a message_id-specific redis key that will enable the feeder to enrich engagement events
	if msgIDindex, ok := hdrs[msgIDField]; !ok {
		c.ConsoleAndLogFatal("Error: redis key", c.RedisAcctHeaders, "does not contain mapping for header_x-sp-message-id")
	} else {
		msgIDKey := c.TrackingPrefix + r[msgIDindex]
		enrichment := make(map[string]string)
		for k, i := range hdrs {
			if k != msgIDField && k != typeField {
				enrichment[k] = r[i]
			}
		}
		// Set key message_id in Redis
		enrichmentJSON, err := json.Marshal(enrichment)
		c.Check(err)
		_, err = client.Set(msgIDKey, enrichmentJSON, ttl).Result()
		c.Check(err)
		log.Println("Loaded", msgIDKey, "->", string(enrichmentJSON), "into Redis")
	}
}

func main() {
	c.MyLogger("acct_etl.log")
	flag.Parse()
	var f *os.File
	var err error
	// Check if input file specified on command-line args, or using stdin
	switch flag.NArg() {
	case 0:
		f = os.Stdin
	case 1:
		f, err = os.Open(flag.Arg(0))
		c.Check(err)
	default:
		c.ConsoleAndLogFatal("Command line args: input must be from stdin or file")
	}
	inbuf := bufio.NewReader(f)

	client := c.MyRedis()
	input := csv.NewScanner(inbuf)
	for input.Scan() {
		r := input.Record()
		switch r[0] {
		case deliveryType:
			storeEvent(r, client)
		case typeField:
			storeHeaders(r, client)
		default:
			c.ConsoleAndLogFatal("Accounting record not of type d:", r)
		}
	}
}