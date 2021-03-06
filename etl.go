package sparkypmtatracking

import (
	"encoding/json"
	"fmt"
	"io"
	"log"

	"github.com/go-redis/redis"
	"github.com/smartystreets/scanners/csv"
)

// Scan input accounting records - required fields for augmentation are: type, header_x-sp-message-id.
// Delivery records are of type "d".
// These fields should match your /etc/pmta/config
const typeField = "type"
const msgIDField = "header_x-sp-message-id"
const deliveryType = "d"

var requiredAcctFields = []string{
	typeField, msgIDField,
}
var optionalAcctFields = []string{
	"rcpt", "header_x-sp-subaccount-id",
}

// StoreHeaders puts an acccounting header record (sent at PowerMTA startup).
//   Checks for required and optional fields.
//   Writes these into persistent storage, so that we can decode "d" records in future, separate process invocations.
func StoreHeaders(r []string, client *redis.Client) error {
	log.Printf("PowerMTA accounting headers: %v\n", r)
	hdrs := make(map[string]int)
	for _, f := range requiredAcctFields {
		fpos, found := PositionIn(r, f)
		if found {
			hdrs[f] = fpos
		} else {
			return fmt.Errorf("Required field %s is not present in PMTA accounting headers", f)
		}
	}
	// Pick up positions of optional fields, for event augmentation
	for _, f := range optionalAcctFields {
		fpos, found := PositionIn(r, f)
		if found {
			hdrs[f] = fpos
		}
	}
	hdrsJSON, err := json.Marshal(hdrs)
	if err != nil {
		return err
	}
	_, err = client.Set(RedisAcctHeaders, hdrsJSON, 0).Result()
	if err != nil {
		return err
	}
	log.Println("Loaded", RedisAcctHeaders, "->", string(hdrsJSON), "into Redis")
	return nil
}

// StoreEvent puts a single accounting event r into redis, based on previously seen header format
func StoreEvent(r []string, client *redis.Client) error {
	hdrsJ, err := client.Get(RedisAcctHeaders).Result()
	if err == redis.Nil {
		return fmt.Errorf("Redis key %v not found", RedisAcctHeaders)
	}
	hdrs := make(map[string]int)
	err = json.Unmarshal([]byte(hdrsJ), &hdrs)
	if err != nil {
		return err
	}
	// read fields into a message_id-specific redis key
	msgIDindex, ok := hdrs[msgIDField]
	if !ok {
		return fmt.Errorf("Redis key %v is missing field header_x-sp-message-id", RedisAcctHeaders)
	}
	msgIDKey := TrackingPrefix + r[msgIDindex]
	augment := make(map[string]string)
	for k, i := range hdrs {
		if k != msgIDField && k != typeField {
			augment[k] = r[i]
		}
	}
	// Set key message_id in Redis
	augmentJSON, err := json.Marshal(augment)
	if err != nil {
		return err
	}
	_, err = client.Set(msgIDKey, augmentJSON, MsgIDTTL).Result()
	if err != nil {
		return err
	}
	log.Printf("Loaded %s -> %s into Redis\n", msgIDKey, string(augmentJSON))
	return nil
}

// AccountETL extracts, transforms accounting data from PowerMTA into Redis records
func AccountETL(f io.Reader) error {
	client := MyRedis()
	input := csv.NewScanner(f)
	for input.Scan() {
		r := input.Record()
		if len(r) < len(requiredAcctFields) {
			return fmt.Errorf("Insufficient data fields %v", r)
		}
		switch r[0] {
		case deliveryType:
			if err := StoreEvent(r, client); err != nil {
				return err
			}
		case typeField:
			if err := StoreHeaders(r, client); err != nil {
				return err
			}
		default:
			return fmt.Errorf("Accounting record not of expected type: %v", r)
		}
	}
	return nil
}
