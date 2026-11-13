// Command conform holds recorded JSON responses to the schema (JS-070).
//
//	conform check -schema compat/schema-12.4.json -rules compat/conform.yaml -label "e2e browse" < responses.jsonl
//	    prints every violation and "<label>: checked N JSON responses on M routes"; fails on a violation or when it checked none
//	conform gen -image jellyfin/jellyfin:12.4 < openapi.json > compat/schema-12.4.json
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"jellyscale/compat/conform"
)

func main() {
	if len(os.Args) < 2 {
		fail("usage: conform check|gen [flags]")
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	switch os.Args[1] {
	case "check":
		schemaFile := fs.String("schema", "compat/schema-12.4.json", "")
		rulesFile := fs.String("rules", "compat/conform.yaml", "")
		label := fs.String("label", "conform", "")
		fs.Parse(os.Args[2:])
		file, err := os.ReadFile(*schemaFile)
		if err != nil {
			fail(err.Error())
		}
		schema, err := conform.ParseSchema(file)
		if err != nil {
			fail(err.Error())
		}
		rules, err := conform.LoadRules(*rulesFile)
		if err != nil {
			fail(err.Error())
		}
		c := conform.NewChecker(schema, rules)
		violations := conform.CheckJSONL(os.Stdin, c)
		for _, v := range violations {
			fmt.Println("FAIL ", v)
		}
		summary, err := c.Summary(*label)
		if err != nil {
			fail(err.Error())
		}
		fmt.Println(summary)
		if len(violations) > 0 {
			fail(fmt.Sprintf("%s: %d violations of the schema", *label, len(violations)))
		}
	case "gen":
		image := fs.String("image", "", "the container the document came from")
		fs.Parse(os.Args[2:])
		doc, err := io.ReadAll(os.Stdin)
		if err != nil {
			fail(err.Error())
		}
		out, err := conform.FromOpenAPI(doc, *image)
		if err != nil {
			fail(err.Error())
		}
		os.Stdout.Write(out)
	default:
		fail("usage: conform check|gen [flags]")
	}
}

func fail(msg string) {
	fmt.Fprintln(os.Stderr, "FAIL ", msg)
	os.Exit(1)
}
