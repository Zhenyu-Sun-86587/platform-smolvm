package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"sandbox-platform/internal/tasks"
)

type entry struct {
	Name string `json:"name"`
	Type string `json:"type"`
	Size int64  `json:"size"`
}

func list(path string, out io.Writer) error {
	if !filepath.IsAbs(path) {
		return errors.New("file listing requires an absolute guest path")
	}
	files, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	if len(files) > 10000 {
		return errors.New("directory has more than 10000 entries")
	}
	result := make([]entry, 0, len(files))
	for _, file := range files {
		info, err := file.Info()
		if err != nil {
			return err
		}
		kind := "other"
		switch {
		case info.Mode().IsRegular():
			kind = "file"
		case info.Mode().IsDir():
			kind = "directory"
		case info.Mode()&os.ModeSymlink != 0:
			kind = "symlink"
		}
		result = append(result, entry{Name: file.Name(), Type: kind, Size: info.Size()})
	}
	return json.NewEncoder(out).Encode(result)
}

func run(args []string, in io.Reader, out io.Writer) error {
	if len(args) == 2 && args[0] == "list-files" {
		return list(args[1], out)
	}
	if len(args) < 1 || args[0] != "rpc" {
		return errors.New("usage: taskctl rpc [-socket PATH] or taskctl list-files PATH")
	}
	flags := flag.NewFlagSet("rpc", flag.ContinueOnError)
	socket := flags.String("socket", tasks.SocketPath, "guest control socket")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected RPC argument")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	reply, err := tasks.Send(ctx, *socket, io.LimitReader(in, (2<<20)+1))
	if err != nil && reply.Error == nil {
		return err
	}
	return json.NewEncoder(out).Encode(reply)
}

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
