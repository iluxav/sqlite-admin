//go:build !linux && !darwin

package cli

import "errors"

var errDaemonUnsupported = errors.New("background management requires Linux or macOS; use the serve command with your service manager")

func serveManaged(o options, ready func(instance) error) error { return serve(o, nil, ready) }
func daemonChild(options) error                                { return errDaemonUnsupported }
func startDaemon(options) (instance, error)                    { return instance{}, errDaemonUnsupported }
func daemonStatus(options) (instance, error)                   { return instance{}, errDaemonUnsupported }
func stopDaemon(options) error                                 { return errDaemonUnsupported }
