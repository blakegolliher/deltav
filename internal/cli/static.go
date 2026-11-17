package cli

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	"deltav/internal/xds"
)

const staticTargetSyntax = "NAME:LISTENPORT:IP:PORT[,IP:PORT...]"

// parseStaticTargets parses --static-target values. Each value holds one
// target, or several separated by ";" so one environment variable can carry
// them all.
func parseStaticTargets(values []string) ([]xds.TargetSpec, error) {
	var out []xds.TargetSpec
	for _, v := range values {
		for _, one := range strings.Split(v, ";") {
			if one = strings.TrimSpace(one); one == "" {
				continue
			}
			t, err := parseStaticTarget(one)
			if err != nil {
				return nil, err
			}
			out = append(out, t)
		}
	}
	return out, nil
}

// parseStaticTarget parses "s3:9000:127.0.0.11:9100,127.0.0.12:9100": the
// target s3, listening on 127.0.0.1:9000 on every client, balancing across
// the two endpoints. IPv6 endpoints are written [addr]:port.
func parseStaticTarget(s string) (xds.TargetSpec, error) {
	bad := func(why string) (xds.TargetSpec, error) {
		return xds.TargetSpec{}, fmt.Errorf("static target %q: %s (want %s)", s, why, staticTargetSyntax)
	}
	head := strings.SplitN(s, ":", 3)
	if len(head) != 3 || head[0] == "" {
		return bad("missing name, listen port or endpoint")
	}
	listen, err := strconv.ParseUint(head[1], 10, 16)
	if err != nil || listen == 0 {
		return bad("listen port must be 1-65535")
	}
	t := xds.TargetSpec{Name: head[0], ListenPort: uint32(listen)}
	for _, ep := range strings.Split(head[2], ",") {
		host, portStr, err := net.SplitHostPort(strings.TrimSpace(ep))
		if err != nil {
			return bad(fmt.Sprintf("endpoint %q is not IP:PORT", ep))
		}
		port, err := strconv.ParseUint(portStr, 10, 16)
		if err != nil || port == 0 || net.ParseIP(host) == nil {
			return bad(fmt.Sprintf("endpoint %q is not IP:PORT", ep))
		}
		t.Endpoints = append(t.Endpoints, xds.Endpoint{IP: host, Port: uint32(port)})
	}
	return t, nil
}
