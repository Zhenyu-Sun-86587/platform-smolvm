package main

import (
	"context"
	"flag"
	"log"
	"os/signal"
	"syscall"

	"sandbox-platform/internal/tasks"
)

func main() {
	config := tasks.DaemonConfig{}
	flag.StringVar(&config.StateDir, "state-dir", tasks.DefaultStateDir, "guest state directory")
	flag.StringVar(&config.Socket, "socket", tasks.SocketPath, "guest control socket")
	flag.StringVar(&config.Runner, "runner", "/usr/local/bin/task-runner", "runner executable")
	flag.StringVar(&config.CgroupRoot, "cgroup-root", "/sys/fs/cgroup", "guest cgroup v2 mount")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err := tasks.Serve(ctx, config); err != nil {
		log.Fatal(err)
	}
}
