package main

import (
	"github.com/sourcegraph/sourcegraph/cmd/pings/service"
	"github.com/sourcegraph/sourcegraph/lib/managedservicesplatform/runtime"
)

func main() {
	runtime.Start[service.Config](&service.Service{})
}
