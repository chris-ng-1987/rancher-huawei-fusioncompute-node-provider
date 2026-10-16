package main

import (
	"github.com/docker/machine/libmachine/drivers/plugin"
)

// DriverName is the name the user passes to `docker-machine create -d <name>`.
const DriverName = "fusioncompute"

func main() {
	plugin.RegisterDriver(NewDriver("", ""))
}
