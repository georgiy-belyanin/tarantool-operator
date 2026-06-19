package tarantool3

import (
	"context"
	"fmt"
	"strings"
	"time"

	tarantool "github.com/tarantool/go-tarantool/v2"

	"github.com/tarantool/tarantool-operator/apis/v2alpha1"
)

// Shared iproto plumbing: every controller-side conversation with a running
// instance (leader observation, config reload, schema upgrade) goes through
// the one evalInstanceString seam below.

// connectTimeout bounds each instance dial so a single unreachable instance
// cannot stall a reconcile.
const connectTimeout = 2 * time.Second

// observeBudget bounds the TOTAL time a per-instance sweep may spend across
// all instances, so an unreachable replica set cannot block a reconcile for
// replicas × connectTimeout. The operations are best-effort: when the budget
// is exhausted the next reconcile retries.
const observeBudget = 5 * time.Second

// instanceAddr is the iproto address of the ordinal-th instance.
func instanceAddr(cluster *v2alpha1.Cluster, rs *v2alpha1.ReplicaSet, ordinal, port int32) string {
	return fmt.Sprintf("%s.%s.%s.svc.%s:%d",
		rs.InstanceName(ordinal), cluster.Name, rs.Namespace, cluster.GetDomain(), port)
}

// forEachInstance runs fn against the first n instance addresses under a shared
// budget context (budget caps the whole sweep; each dial is further capped by
// connectTimeout inside the seam). Callers pass rs.GetReplicas() to sweep the
// whole set, or the StatefulSet's current size to address only instances that
// exist (the scale-up gate). fn returns true to stop the sweep early.
func forEachInstance(ctx context.Context, cluster *v2alpha1.Cluster, rs *v2alpha1.ReplicaSet, port int32, n int32, budget time.Duration, fn func(ctx context.Context, addr string) (stop bool)) {
	sweepCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	for ordinal := int32(0); ordinal < n; ordinal++ {
		if sweepCtx.Err() != nil {
			return // budget exhausted; best-effort, retry next reconcile
		}
		if fn(sweepCtx, instanceAddr(cluster, rs, ordinal, port)) {
			return
		}
	}
}

// evalInstanceString connects to addr and evaluates a script returning a string.
// Package var so tests can substitute the network call — the ONE test seam for
// all controller-side iproto traffic.
var evalInstanceString = func(ctx context.Context, addr, user, password, script string, reqTimeout time.Duration) (string, error) {
	connCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()

	conn, err := tarantool.Connect(connCtx, tarantool.NetDialer{
		Address:  addr,
		User:     user,
		Password: password,
	}, tarantool.Opts{Timeout: reqTimeout})
	if err != nil {
		return "", err
	}
	defer conn.Close()

	data, err := conn.Do(tarantool.NewEvalRequest(script)).Get()
	if err != nil {
		return "", err
	}
	if len(data) == 0 {
		return "", nil
	}
	s, _ := data[0].(string)
	return s, nil
}

// readElectedLeader reads the elected leader's instance name from addr.
func readElectedLeader(ctx context.Context, addr, user, password string) (string, error) {
	return evalInstanceString(ctx, addr, user, password, leaderEvalScript, connectTimeout)
}

// reloadInstanceConfig runs the reload+verify script on addr. It returns the
// config version the instance ended up running and — when the instance could not
// cleanly apply the desired config — Tarantool's own explanation (the
// config:reload() error or the config alerts), "" otherwise.
func reloadInstanceConfig(ctx context.Context, addr, user, password, desiredHash string) (loaded, applyErr string, err error) {
	out, err := evalInstanceString(ctx, addr, user, password, fmt.Sprintf(reloadEvalScript, desiredHash), connectTimeout)
	if err != nil {
		return "", "", err
	}
	loaded, applyErr, _ = strings.Cut(out, "\t")
	return loaded, applyErr, nil
}
