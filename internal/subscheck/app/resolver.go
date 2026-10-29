package app

import (
	"fmt"
	"log/slog"
	"net"
	"strings"

	"substore-ui-go/internal/subscheck/config"
	"github.com/metacubex/mihomo/component/resolver"
	_ "github.com/metacubex/mihomo/config"
	"github.com/metacubex/mihomo/dns"
)

var defaultBootstrapNameservers = []string{
	"223.5.5.5",
	"119.29.29.29",
}

func initResolver() error {
	c := &config.GlobalConfig.DNS
	resolver.DisableIPv6 = !config.GlobalConfig.IPv6

	if !c.Enable {
		return nil
	}

	if len(c.DefaultNameserver) == 0 {
		c.DefaultNameserver = defaultBootstrapNameservers
	}
	valid, err := validateBootstrapIPs(c.DefaultNameserver)
	if err != nil {
		return err
	}
	c.DefaultNameserver = valid
	if len(c.Nameserver) == 0 {
		c.Nameserver = c.DefaultNameserver
	}
	if len(c.ProxyServerNameserver) == 0 {
		c.ProxyServerNameserver = c.Nameserver
	}

	main, err := parseNameservers(c.Nameserver, "nameserver")
	if err != nil {
		return err
	}
	proxySrv, err := parseNameservers(c.ProxyServerNameserver, "proxy-server-nameserver")
	if err != nil {
		return err
	}
	def, err := parseNameservers(c.DefaultNameserver, "default-nameserver")
	if err != nil {
		return err
	}

	rs := dns.NewResolver(dns.Config{
		Main:        main,
		Default:     def,
		ProxyServer: proxySrv,
		IPv6:        config.GlobalConfig.IPv6,
	})

	resolver.DefaultResolver = rs.Resolver
	resolver.ProxyServerHostResolver = rs.ProxyResolver

	slog.Info("DNS resolver 使用自定义 DNS",
		"nameserver", len(main),
		"proxy-server", len(proxySrv),
		"default", len(def),
		"ipv6", config.GlobalConfig.IPv6)
	return nil
}

func parseNameservers(servers []string, fieldName string) ([]dns.NameServer, error) {
	out := make([]dns.NameServer, 0, len(servers))
	for _, s := range servers {
		ns, err := dns.ParseNameServer([]string{s})
		if err != nil {
			slog.Warn(fieldName+" 跳过无效项", "value", s, "reason", err)
			continue
		}
		out = append(out, ns...)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s 全部无效，至少需要一个有效项", fieldName)
	}
	return out, nil
}

func validateBootstrapIPs(servers []string) ([]string, error) {
	valid := make([]string, 0, len(servers))
	for _, ns := range servers {
		host := ns
		if h, _, err := net.SplitHostPort(ns); err == nil {
			host = h
		}
		host = strings.TrimPrefix(strings.TrimSuffix(host, "]"), "[")
		if net.ParseIP(host) == nil {
			slog.Warn("default-nameserver 跳过无效 IP", "value", ns)
			continue
		}
		valid = append(valid, ns)
	}
	if len(valid) == 0 {
		return nil, fmt.Errorf("default-nameserver 全部无效，至少需要一个有效 IP")
	}
	return valid, nil
}
