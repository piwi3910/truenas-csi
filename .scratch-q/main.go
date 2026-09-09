package main

import (
	"context"
	"encoding/json"
	"os"

	"github.com/piwi3910/truenas-csi/internal/config"
	"github.com/piwi3910/truenas-csi/internal/truenas"
)

func main() {
	b := config.Backend{Name: "nas1", Endpoint: os.Getenv("TRUENAS_ENDPOINT"),
		Username: "truenas_admin", APIKey: os.Getenv("TRUENAS_API_KEY"),
		Pool: "Pool0", ParentDataset: "k8s", InsecureSkipVerify: true}
	ctx := context.Background()
	c, err := truenas.Dial(ctx, b)
	if err != nil {
		panic(err)
	}
	defer c.Close()
	var params []any
	if len(os.Args) > 2 {
		_ = json.Unmarshal([]byte(os.Args[2]), &params)
	}
	var raw json.RawMessage
	if err := (&truenas.Ops{Transport: c}).CallJSON(ctx, &raw, os.Args[1], params...); err != nil {
		panic(err)
	}
	var v any
	_ = json.Unmarshal(raw, &v)
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", " ")
	_ = enc.Encode(v)
}
