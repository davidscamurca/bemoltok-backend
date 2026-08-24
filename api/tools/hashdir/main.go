// Command hashdir converts a raw email;id_cliente directory into a privacy-
// preserving artifact keyed by HMAC-SHA256(secret, email). The raw email never
// leaves the machine that runs this tool: only the hashed file is meant to be
// uploaded to GCS and loaded by the recommendation service.
//
// Usage:
//
//	export DIRECTORY_HMAC_SECRET=<64-hex-secret>
//	go run . -in ../../artifacts/email_cli.csv -out ../../artifacts/email_cli.hashed.csv
//
// Output format (no header): <email_hmac_hex>;<id_cliente>
//
// Email is normalized as lower(trim(email)) before hashing — the service must
// apply the exact same normalization when looking up the token's email.
package main

import (
	"bufio"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
)

func main() {
	in := flag.String("in", "", "input CSV path (email;id_cliente, optional header)")
	out := flag.String("out", "", "output CSV path (email_hmac;id_cliente)")
	secret := flag.String("secret", os.Getenv("DIRECTORY_HMAC_SECRET"), "HMAC secret (defaults to $DIRECTORY_HMAC_SECRET)")
	sep := flag.String("sep", ";", "field separator")
	flag.Parse()

	if *in == "" || *out == "" {
		log.Fatal("usage: hashdir -in <raw.csv> -out <hashed.csv> [-secret <hex>] (or set DIRECTORY_HMAC_SECRET)")
	}
	if *secret == "" {
		log.Fatal("missing HMAC secret: pass -secret or set DIRECTORY_HMAC_SECRET")
	}
	if len([]rune(*sep)) != 1 {
		log.Fatalf("separator must be a single character, got %q", *sep)
	}

	f, err := os.Open(*in)
	if err != nil {
		log.Fatalf("open input: %v", err)
	}
	defer f.Close()

	r := csv.NewReader(f)
	r.Comma = []rune(*sep)[0]
	r.FieldsPerRecord = -1 // tolerate ragged rows; we validate per-row below.

	of, err := os.Create(*out)
	if err != nil {
		log.Fatalf("create output: %v", err)
	}
	defer of.Close()
	w := bufio.NewWriter(of)
	defer w.Flush()

	key := []byte(*secret)
	seen := make(map[string]struct{})

	var total, written, skipped, dups int
	first := true
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Fatalf("read row %d: %v", total+1, err)
		}
		total++

		// Skip a header row if present.
		if first {
			first = false
			if len(rec) >= 1 && strings.EqualFold(strings.TrimSpace(rec[0]), "email") {
				total--
				continue
			}
		}

		if len(rec) < 2 {
			skipped++
			continue
		}
		email := strings.ToLower(strings.TrimSpace(rec[0]))
		id := strings.TrimSpace(rec[1])
		if email == "" || id == "" {
			skipped++
			continue
		}
		if _, ok := seen[email]; ok {
			dups++
			continue // first occurrence wins
		}
		seen[email] = struct{}{}

		mac := hmac.New(sha256.New, key)
		mac.Write([]byte(email))
		sum := hex.EncodeToString(mac.Sum(nil))

		if _, err := fmt.Fprintf(w, "%s;%s\n", sum, id); err != nil {
			log.Fatalf("write: %v", err)
		}
		written++
	}

	log.Printf("done: total=%d written=%d skipped=%d duplicates=%d -> %s",
		total, written, skipped, dups, *out)
	if dups > 0 {
		log.Printf("WARNING: %d duplicate email(s) found; first occurrence kept", dups)
	}
}
