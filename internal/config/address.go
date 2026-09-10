package config

import (
	"net"
	"strconv"
)

// Require reports a FieldError when value is empty.
func Require(field, value string) error {
	if value == "" {
		return Invalid(field, "is required")
	}
	return nil
}

// ListenAddr checks that value is a "host:port" a listener can bind. The host
// may be empty for all interfaces, an IP, or a name. The port may be 0 to
// 65535; 0 asks the kernel for a free port, which tests rely on.
func ListenAddr(field, value string) error {
	_, port, err := splitAddr(field, value)
	if err != nil {
		return err
	}
	if port < 0 || port > 65535 {
		return Invalid(field, "port %d is outside 0 to 65535", port)
	}
	return nil
}

// DialAddr checks that value names a reachable "host:port": the host is
// required and the port is 1 to 65535.
//
// Names are deliberately not resolved here. Resolution happens at dial time,
// so a DNS blip while the service starts does not make a valid configuration
// look broken, and validation stays fast and offline.
func DialAddr(field, value string) error {
	host, port, err := splitAddr(field, value)
	if err != nil {
		return err
	}
	if host == "" {
		return Invalid(field, "needs a host to connect to, got %q", value)
	}
	if port < 1 || port > 65535 {
		return Invalid(field, "port %d is outside 1 to 65535", port)
	}
	return nil
}

// splitAddr separates "host:port" and parses the port as an integer, turning
// every failure into a FieldError that quotes what the operator wrote.
func splitAddr(field, value string) (host string, port int, err error) {
	if value == "" {
		return "", 0, Invalid(field, "is required")
	}
	host, portText, splitErr := net.SplitHostPort(value)
	if splitErr != nil {
		return "", 0, Invalid(field, "%q is not in host:port form", value)
	}
	port, convErr := strconv.Atoi(portText)
	if convErr != nil {
		return "", 0, Invalid(field, "port %q is not a number", portText)
	}
	return host, port, nil
}
