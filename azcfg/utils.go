package azcfg

import (
	"net/url"

	"oss.nandlabs.io/golly/textutils"
)

// GetConfig resolves a Config by URL or fallback name.
// It tries the URL host first, then host+path, and finally the fallback name.
func GetConfig(u *url.URL, name string) (cfg *Config) {
	if u == nil {
		cfg = Manager.Get(name)
		return
	}

	key := u.Host
	cfg = Manager.Get(key)
	if cfg == nil && u.Path != "" {
		cfg = Manager.Get(key + textutils.ForwardSlashStr + u.Path)
	}
	if cfg == nil {
		cfg = Manager.Get(name)
	}
	return
}

// GetConfigOrDefault resolves a Config like GetConfig, falling back to the
// configuration registered as DefaultConfigName and finally to a fresh
// NewConfig (DefaultAzureCredential) when nothing is registered.
func GetConfigOrDefault(u *url.URL, name string) *Config {
	if cfg := GetConfig(u, name); cfg != nil {
		return cfg
	}
	if cfg := Manager.Get(DefaultConfigName); cfg != nil {
		return cfg
	}
	return NewConfig()
}
