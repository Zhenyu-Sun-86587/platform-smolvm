package main

import (
	"log"
	"os"

	"sandbox-platform/internal/tasks"
)

func main() {
	if len(os.Args) != 5 {
		log.Fatal("usage: task-runner {run|launch} STATE_DIR RUN_ID CGROUP_ROOT")
	}
	var err error
	switch os.Args[1] {
	case "run":
		err = tasks.Run(os.Args[2], os.Args[3], os.Args[4])
	case "launch":
		err = tasks.LaunchPayload(os.Args[2], os.Args[3], os.Args[4])
	default:
		log.Fatal("invalid task-runner mode")
	}
	if err != nil {
		log.Fatal(err)
	}
}
