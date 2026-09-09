package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/piwi3910/truenas-csi/internal/config"
	"github.com/piwi3910/truenas-csi/internal/truenas"
)

func main() {
	b := config.Backend{
		Name: "nas1", Endpoint: os.Getenv("TRUENAS_ENDPOINT"),
		Username: "truenas_admin", APIKey: os.Getenv("TRUENAS_API_KEY"),
		Pool: "Pool0", ParentDataset: "k8s", InsecureSkipVerify: true,
	}
	ctx := context.Background()
	c, err := truenas.Dial(ctx, b)
	if err != nil {
		panic(err)
	}
	defer c.Close()
	var params []any
	if len(os.Args) > 2 {
		if err := json.Unmarshal([]byte(os.Args[2]), &params); err != nil {
			panic(err)
		}
	}
	var raw json.RawMessage
	if err := (&truenas.Ops{Transport: c}).CallJSON(ctx, &raw, os.Args[1], params...); err != nil {
		fmt.Fprintln(os.Stderr, "ERR:", err)
		os.Exit(3)
	}
	var v any
	_ = json.Unmarshal(raw, &v)
	e := json.NewEncoder(os.Stdout)
	e.SetIndent("", "  ")
	_ = e.Encode(v)
}
