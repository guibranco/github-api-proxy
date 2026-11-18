// Package systemd implements the two sd_notify messages the service needs
// (READY=1 and STOPPING=1) so the unit can use Type=notify and a deploy only
// counts as successful once the proxy is actually listening.
package systemd

import (
	"net"
	"os"
)

// Notify sends a state string to systemd. It is a no-op outside systemd.
func Notify(state string) error {
	socket := os.Getenv("NOTIFY_SOCKET")
	if socket == "" {
		return nil
	}
	if socket[0] == '@' {
		socket = "\x00" + socket[1:] // abstract namespace
	}
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: socket, Net: "unixgram"})
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = conn.Write([]byte(state))
	return err
}
