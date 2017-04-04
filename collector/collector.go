/*
http://www.apache.org/licenses/LICENSE-2.0.txt
Copyright 2016 Intel Corporation
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at
    http://www.apache.org/licenses/LICENSE-2.0
Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package collector

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/rackspace/gophercloud"

	"github.com/intelsdi-x/snap/control/plugin"
	"github.com/intelsdi-x/snap/control/plugin/cpolicy"
	"github.com/intelsdi-x/snap/core"

	"github.com/intelsdi-x/snap-plugin-utilities/config"
	"github.com/intelsdi-x/snap-plugin-utilities/ns"
	"github.com/intelsdi-x/snap-plugin-utilities/str"

	openstackintel "github.com/intelsdi-x/snap-plugin-collector-cinder/openstack"
	"github.com/intelsdi-x/snap-plugin-collector-cinder/openstack/services"
	"github.com/intelsdi-x/snap-plugin-collector-cinder/types"
)

const (
	name    = "cinder"
	version = 3
	plgtype = plugin.CollectorPluginType
	vendor  = "intel"
	fs      = "openstack"
)

// New creates initialized instance of Cinder collector
func New() *collector {
	providers := map[string]*gophercloud.ProviderClient{}
	allTenants := map[string]string{}
	allLimits := map[string]types.Limits{}
	return &collector{
		allTenants: allTenants,
		providers:  providers,
		allLimits:  allLimits,
	}
}

// GetMetricTypes returns list of available metric types
// It returns error in case retrieval was not successful
func (c *collector) GetMetricTypes(cfg plugin.ConfigType) ([]plugin.MetricType, error) {
	mts := []plugin.MetricType{}

	var err error
	c.allTenants, err = getTenants(cfg)
	if err != nil {
		return nil, err
	}

	// Generate available namespace for limits
	namespaces := []string{}
	for _, tenantName := range c.allTenants {
		// Construct temporary struct to generate namespace based on tags
		var metrics struct {
			S types.Snapshots `json:"snapshots"`
			V types.Volumes   `json:"volumes"`
			L types.Limits    `json:"limits"`
		}
		current := strings.Join([]string{vendor, fs, name, tenantName}, "/")
		ns.FromCompositionTags(metrics, current, &namespaces)
	}

	for _, namespace := range namespaces {
		mts = append(mts, plugin.MetricType{
			Namespace_: core.NewNamespace(strings.Split(namespace, "/")...),
			Config_:    cfg.ConfigDataNode,
		})
	}

	return mts, nil
}

// CollectMetrics returns list of requested metric values
// It returns error in case retrieval was not successful
func (c *collector) CollectMetrics(metricTypes []plugin.MetricType) ([]plugin.MetricType, error) {
	// get admin tenant from configuration. admin tenant is needed for gathering volumes and snapshots metrics at once
	item, err := config.GetConfigItem(metricTypes[0], "tenant")
	if err != nil {
		return nil, err
	}
	admin := item.(string)

	// populate information about all available tenants
	if len(c.allTenants) == 0 {
		c.allTenants, err = getTenants(metricTypes[0])
		if err != nil {
			return nil, err
		}
	}

	// iterate over metric types to resolve needed collection calls
	// for requested tenants
	collectTenants := str.InitSet()
	var collectLimits, collectVolumes, collectSnapshots bool
	for _, metricType := range metricTypes {
		namespace := metricType.Namespace()
		if len(namespace) < 6 {
			return nil, fmt.Errorf("Incorrect namespace lenth. Expected 6 is %d", len(namespace))
		}

		tenant := namespace[3].Value
		collectTenants.Add(tenant)

		if str.Contains(namespace.Strings(), "limits") {
			collectLimits = true
		} else if str.Contains(namespace.Strings(), "volumes") {
			collectVolumes = true
		} else {
			collectSnapshots = true
		}
	}

	allSnapshots := map[string]types.Snapshots{}
	allVolumes := map[string]types.Volumes{}

	// collect volumes and snapshots separately by authenticating to admin
	{
		if err := c.authenticate(metricTypes[0], admin); err != nil {
			return nil, err
		}
		provider := c.providers[admin]

		var done sync.WaitGroup
		errChn := make(chan error, 2)

		// Collect volumes
		if collectVolumes {
			done.Add(1)
			go func() {
				defer done.Done()
				volumes, err := c.service.GetVolumes(provider)

				if err != nil {
					errChn <- err
				}
				for tenantId, volumeCount := range volumes {
					tenantName := c.allTenants[tenantId]
					allVolumes[tenantName] = volumeCount
				}
			}()
		}
		// Collect snapshots
		if collectSnapshots {
			done.Add(1)
			go func() {
				defer done.Done()
				snapshots, err := c.service.GetSnapshots(provider)
				if err != nil {
					errChn <- err
				}

				for tenantId, snapshotCount := range snapshots {
					tenantName := c.allTenants[tenantId]
					allSnapshots[tenantName] = snapshotCount
				}
			}()
		}

		done.Wait()
		close(errChn)

		if e := <-errChn; e != nil {
			return nil, e
		}
	}

	// Collect limits per each tenant only if not already collected (plugin lifetime scope)
	{
		var done sync.WaitGroup
		errChn := make(chan error, collectTenants.Size())

		for _, tenant := range collectTenants.Elements() {
			_, found := c.allLimits[tenant]
			if collectLimits && !found {
				if err := c.authenticate(metricTypes[0], tenant); err != nil {
					return nil, err
				}

				provider := c.providers[tenant]

				done.Add(1)
				go func(p *gophercloud.ProviderClient, t string) {
					defer done.Done()
					limits, err := c.service.GetLimits(p)
					if err != nil {
						errChn <- err
					}
					c.allLimits[t] = limits
				}(provider, tenant)
			}
		}

		done.Wait()
		close(errChn)

		if e := <-errChn; e != nil {
			return nil, e
		}
	}

	metrics := []plugin.MetricType{}
	for _, metricType := range metricTypes {
		namespace := metricType.Namespace().Strings()
		tenant := namespace[3]
		// Construct temporary struct to accommodate all gathered metrics
		metricContainer := struct {
			S types.Snapshots `json:"snapshots"`
			V types.Volumes   `json:"volumes"`
			L types.Limits    `json:"limits"`
		}{
			allSnapshots[tenant],
			allVolumes[tenant],
			c.allLimits[tenant],
		}

		// Extract values by namespace from temporary struct and create metrics
		metric := plugin.MetricType{
			Timestamp_: time.Now(),
			Namespace_: metricType.Namespace(),
			Data_:      ns.GetValueByNamespace(metricContainer, namespace[4:]),
		}
		metrics = append(metrics, metric)
	}

	return metrics, nil
}

// GetConfigPolicy returns config policy
// It returns error in case retrieval was not successful
func (c *collector) GetConfigPolicy() (*cpolicy.ConfigPolicy, error) {
	cp := cpolicy.New()
	return cp, nil
}

// Commenting exported items is very important
func Meta() *plugin.PluginMeta {
	return plugin.NewPluginMeta(
		name,
		version,
		plgtype,
		[]string{plugin.SnapGOBContentType},
		[]string{plugin.SnapGOBContentType},
		plugin.RoutingStrategy(plugin.StickyRouting),
	)
}

type collector struct {
	allTenants map[string]string
	service    services.Service
	common     openstackintel.Commoner
	allLimits  map[string]types.Limits
	providers  map[string]*gophercloud.ProviderClient
}

func (c *collector) authenticate(cfg interface{}, tenant string) error {
	if _, found := c.providers[tenant]; !found {
		domain_name := ""
		domain_id := ""
		// get credentials and endpoint from configuration
		items, err := config.GetConfigItems(cfg, "endpoint", "user", "password")
		if err != nil {
			return err
		}

		endpoint := items["endpoint"].(string)
		user := items["user"].(string)
		password := items["password"].(string)
		dom_name, _ := config.GetConfigItem(cfg, "domain_name")
		dom_id, _ := config.GetConfigItem(cfg, "domain_id")
		if dom_name != nil {
			domain_name = dom_name.(string)
		}
		if dom_id != nil {
			domain_id = dom_id.(string)
		}

		provider, err := openstackintel.Authenticate(endpoint, user, password, tenant, domain_name, domain_id)
		if err != nil {
			return err
		}
		// set provider and dispatch API version based on priority
		c.providers[tenant] = provider
		c.service = services.Dispatch(provider)

		// set Commoner interface
		c.common = openstackintel.Common{}
	}

	return nil
}

func getTenants(cfg interface{}) (map[string]string, error) {
	items, err := config.GetConfigItems(cfg, "endpoint", "user", "password")
	domain_name := ""
	domain_id := ""
	if err != nil {
		return nil, err
	}

	endpoint := items["endpoint"].(string)
	user := items["user"].(string)
	password := items["password"].(string)
	dom_name, _ := config.GetConfigItem(cfg, "domain_name")
	dom_id, _ := config.GetConfigItem(cfg, "domain_id")
	if dom_name != nil {
		domain_name = dom_name.(string)
	}
	if dom_id != nil {
		domain_id = dom_id.(string)
	}

	// retrieve list of all available tenants for provided endpoint, user and password
	cmn := openstackintel.Common{}
	allTenants, err := cmn.GetTenants(endpoint, user, password, domain_name, domain_id)
	if err != nil {
		return nil, err
	}

	return allTenants, nil
}
