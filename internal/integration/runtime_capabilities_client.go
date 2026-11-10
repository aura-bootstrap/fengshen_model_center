package integration

import (
	"context"
	"time"
)

// RuntimeCapabilities reads existing metadata without launching the center or verifying a new build.
func (c *Client) RuntimeCapabilities(ctx context.Context) (RuntimeCapabilities, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	c.mu.Lock()
	defer c.mu.Unlock()
	var v RuntimeCapabilities
	h, e := c.connect(ctx, false)
	if e != nil {
		return v, e
	}
	if e = fetchLimited(ctx, h, RuntimeCapabilitiesPath, &v, 3*time.Second, MaxResponse); e == nil {
		e = validateRuntimeCapabilities(v, h.Descriptor.InstanceID, h.Build)
	}
	if e != nil {
		c.cache = cacheEntry{}
		return RuntimeCapabilities{}, e
	}
	return v, nil
}
