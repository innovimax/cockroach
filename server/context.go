// Copyright 2015 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License. See the AUTHORS file
// for names of contributors.
//
// Author: Daniel Theophanes (kardianos@gmail.com)

package server

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cockroachdb/cockroach/client"
	"github.com/cockroachdb/cockroach/gossip"
	"github.com/cockroachdb/cockroach/proto"
	"github.com/cockroachdb/cockroach/storage/engine"
	"github.com/cockroachdb/cockroach/util"
	"github.com/cockroachdb/cockroach/util/log"
)

// Context defaults.
const (
	defaultAddr           = ":8080"
	defaultCertsDir       = "certs"
	defaultMaxOffset      = 250 * time.Millisecond
	defaultGossipInterval = 2 * time.Second
	defaultCacheSize      = 1 << 30 // GB
	defaultScanInterval   = 10 * time.Minute
)

// Context holds parameters needed to setup a server.
// Calling "server/cli".InitFlags(ctx *Context) will initialize Context using
// command flags. Keep in sync with "server/cli/flags.go".
type Context struct {
	// Addr is the host:port to bind for HTTP/RPC traffic.
	Addr string

	// Certs specifies a directory containing RSA key and x509 certs.
	Certs string

	// Stores is specified to enable durable key-value storage.
	// Memory-backed key value stores may be optionally specified
	// via mem=<integer byte size>.
	//
	// Stores specify a comma-separated list of stores specified by a
	// colon-separated list of device attributes followed by '=' and
	// either a filepath for a persistent store or an integer size in bytes for an
	// in-memory store. Device attributes typically include whether the store is
	// flash (ssd), spinny disk (hdd), fusion-io (fio), in-memory (mem); device
	// attributes might also include speeds and other specs (7200rpm, 200kiops, etc.).
	// For example, -store=hdd:7200rpm=/mnt/hda1,ssd=/mnt/ssd01,ssd=/mnt/ssd02,mem=1073741824
	Stores string

	// Attrs specifies a colon-separated list of node topography or machine
	// capabilities, used to match capabilities or location preferences specified
	// in zone configs.
	Attrs string

	// Maximum clock offset for the cluster.
	MaxOffset time.Duration

	// GossipBootstrap is a comma-separated list of node addresses that
	// act as bootstrap hosts for connecting to the gossip network.
	GossipBootstrap string

	// GossipInterval is a time interval specifying how often gossip is
	// communicated between hosts on the gossip network.
	GossipInterval time.Duration

	// Enables linearizable behaviour of operations on this node by making sure
	// that no commit timestamp is reported back to the client until all other
	// node clocks have necessarily passed it.
	Linearizable bool

	// CacheSize is the amount of memory in bytes to use for caching data.
	// The value is split evenly between the stores if there are more than one.
	CacheSize int64

	// Parsed values.

	// Engines is the storage instances specified by Stores.
	Engines []engine.Engine

	// NodeAttributes is the parsed representation of Attrs.
	NodeAttributes proto.Attributes

	// GossipBootstrapResolvers is a list of gossip resolvers used
	// to find bootstrap nodes for connecting to the gossip network.
	GossipBootstrapResolvers []gossip.Resolver

	// ScanInterval determines a duration during which each range should be
	// visited approximately once by the range scanner.
	ScanInterval time.Duration

	// httpClient is a lazily-initialized http client.
	// It should be accessed through Context.GetHTTPClient() which will
	// initialize if needed.
	httpClient *http.Client
	// Protecs httpClient.
	httpClientMu sync.Mutex
}

// NewContext returns a Context with default values.
func NewContext() *Context {
	return &Context{
		Addr:           defaultAddr,
		Certs:          defaultCertsDir,
		MaxOffset:      defaultMaxOffset,
		GossipInterval: defaultGossipInterval,
		CacheSize:      defaultCacheSize,
		ScanInterval:   defaultScanInterval,
	}
}

// Init interprets the stores parameter to initialize a slice of
// engine.Engine objects, parses node attributes, and initializes
// the gossip bootstrap resolvers.
func (ctx *Context) Init() error {
	var err error
	storesRE := regexp.MustCompile(`([^=]+)=([^,]+)(,|$)`)
	// Error if regexp doesn't match.
	storeSpecs := storesRE.FindAllStringSubmatch(ctx.Stores, -1)
	if storeSpecs == nil || len(storeSpecs) == 0 {
		return fmt.Errorf("invalid or empty engines specification %q, "+
			"did you specify -stores?", ctx.Stores)
	}

	ctx.Engines = nil
	for _, store := range storeSpecs {
		if len(store) != 4 {
			return util.Errorf("unable to parse attributes and path from store %q", store[0])
		}
		// There are two matches for each store specification: the colon-separated
		// list of attributes and the path.
		engine, err := ctx.initEngine(store[1], store[2])
		if err != nil {
			return util.Errorf("unable to init engine for store %q: %s", store[0], err)
		}
		ctx.Engines = append(ctx.Engines, engine)
	}
	log.Infof("initialized %d storage engine(s)", len(ctx.Engines))

	ctx.NodeAttributes = parseAttributes(ctx.Attrs)

	resolvers, err := ctx.parseGossipBootstrapResolvers()
	if err != nil {
		return err
	}
	if len(resolvers) == 0 {
		return errors.New("no gossip addresses found, did you specify -gossip?")
	}
	ctx.GossipBootstrapResolvers = resolvers

	return nil
}

// initEngine parses the store attributes as a colon-separated list
// and instantiates an engine based on the dir parameter. If dir parses
// to an integer, it's taken to mean an in-memory engine; otherwise,
// dir is treated as a path and a RocksDB engine is created.
func (ctx *Context) initEngine(attrsStr, path string) (engine.Engine, error) {
	attrs := parseAttributes(attrsStr)
	if size, err := strconv.ParseUint(path, 10, 64); err == nil {
		if size == 0 {
			return nil, util.Errorf("unable to initialize an in-memory store with capacity 0")
		}
		return engine.NewInMem(attrs, int64(size)), nil
		// TODO(spencer): should be using rocksdb for in-memory stores and
		// relegate the InMem engine to usage only from unittests.
	}
	return engine.NewRocksDB(attrs, path, ctx.CacheSize), nil
}

// parseGossipBootstrapResolvers parses a comma-separated list of
// gossip bootstrap resolvers.
func (ctx *Context) parseGossipBootstrapResolvers() ([]gossip.Resolver, error) {
	var bootstrapResolvers []gossip.Resolver
	addresses := strings.Split(ctx.GossipBootstrap, ",")
	for _, address := range addresses {
		if len(address) == 0 {
			continue
		}
		// Special case self:// to pick a nice address that resolves
		// uniquely for use in Gossip. This avoids having to specify
		// the port for single-node clusters twice (once in -addr,
		// once in -gossip).
		if strings.HasPrefix(address, "self://") {
			address = util.EnsureHost(ctx.Addr)
		}
		resolver, err := gossip.NewResolver(address)
		if err != nil {
			return nil, err
		}
		bootstrapResolvers = append(bootstrapResolvers, resolver)
	}

	return bootstrapResolvers, nil
}

// GetHTTPClient returns the context http client, initializing it
// if needed. It uses the context Certs.
func (ctx *Context) GetHTTPClient() (*http.Client, error) {
	ctx.httpClientMu.Lock()
	defer ctx.httpClientMu.Unlock()
	var err error
	if ctx.httpClient == nil {
		ctx.httpClient, err = client.NewHTTPClient(ctx.Certs)
	}
	return ctx.httpClient, err
}

// parseAttributes parses a colon-separated list of strings,
// filtering empty strings (i.e. "::" will yield no attributes.
// Returns the list of strings as Attributes.
func parseAttributes(attrsStr string) proto.Attributes {
	var filtered []string
	for _, attr := range strings.Split(attrsStr, ":") {
		if len(attr) != 0 {
			filtered = append(filtered, attr)
		}
	}
	return proto.Attributes{Attrs: filtered}
}
