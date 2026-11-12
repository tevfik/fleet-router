package config

// ChainSupervision: whether the daemon supervises the chains it deployed (supervise_chains, default on).
func (c *Config) ChainSupervision() bool {
	return c == nil || c.SuperviseChains == nil || *c.SuperviseChains
}
