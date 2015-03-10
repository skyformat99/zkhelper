package zkhelper

import (
	"errors"
	"path"
	"strings"
	"time"

	etcderr "github.com/coreos/etcd/error"
	"github.com/coreos/go-etcd/etcd"
	zk "github.com/ngaut/go-zookeeper/zk"
	log "github.com/ngaut/logging"
	"github.com/ngaut/pools"
	"sync"
)

var (
	singleInstanceLock sync.Mutex
	etcdInstance       *etcdImpl
)

type PooledEtcdClient struct {
	c *etcd.Client
}

func (c *PooledEtcdClient) Close() {

}

type etcdImpl struct {
	cluster string
	pool    *pools.ResourcePool
}

func convertToZkError(err error) error {
	//todo:implementation
	if ec, ok := err.(*etcd.EtcdError); ok {
		switch ec.ErrorCode {
		case etcderr.EcodeKeyNotFound:
			return zk.ErrNoNode
		case etcderr.EcodeNotFile:
		case etcderr.EcodeNotDir:
		case etcderr.EcodeNodeExist:
			return zk.ErrNodeExists
		case etcderr.EcodeDirNotEmpty:
			return zk.ErrNotEmpty
		}
	}

	return err
}

func convertToZkEvent(resp *etcd.Response, err error) zk.Event {
	log.Infof("convert event %+v, %+v", resp, resp.Node)

	var e zk.Event

	if err != nil {
		e.Err = convertToZkError(err)
		e.State = zk.StateDisconnected
		return e
	}

	e.State = zk.StateConnected

	switch resp.Action {
	case "set":
		e.Type = zk.EventNodeDataChanged
	case "delete":
		e.Type = zk.EventNodeDeleted
	case "update":
		//todo:check if just ttl changed
		e.Type = zk.EventNodeDataChanged
	case "create":
		e.Type = zk.EventNodeCreated
	}

	e.Path = resp.Node.Key

	return e
}

func NewEtcdConn(zkAddr string) (Conn, error) {
	singleInstanceLock.Lock()
	defer singleInstanceLock.Unlock()
	if etcdInstance != nil {
		return etcdInstance, nil
	}

	p := pools.NewResourcePool(func() (pools.Resource, error) {
		log.Info("create a new etcd client")
		return &PooledEtcdClient{c: etcd.NewClient(strings.Split(zkAddr, ","))}, nil
	}, 10, 10, 0)

	etcdInstance = &etcdImpl{cluster: zkAddr, pool: p}

	log.Infof("new etcd %s", zkAddr)
	if etcdInstance == nil {
		return nil, errors.New("unknown error")
	}

	return etcdInstance, nil
}

func (e *etcdImpl) Get(key string) (data []byte, stat zk.Stat, err error) {
	conn, err := e.pool.Get()
	if err != nil {
		return nil, nil, err
	}

	defer e.pool.Put(conn)
	c := conn.(*PooledEtcdClient).c

	resp, err := c.Get(key, true, false)
	if resp == nil {
		return nil, nil, convertToZkError(err)
	}

	return []byte(resp.Node.Value), nil, nil
}

func (e *etcdImpl) watch(key string, children bool) (resp *etcd.Response, stat zk.Stat, watch <-chan zk.Event, err error) {
	conn, err := e.pool.Get()
	if err != nil {
		return nil, nil, nil, err
	}

	defer e.pool.Put(conn)
	c := conn.(*PooledEtcdClient).c

	resp, err = c.Get(key, true, true)
	if resp == nil {
		return nil, nil, nil, convertToZkError(err)
	}

	maxIndex := resp.Node.ModifiedIndex
	for _, n := range resp.Node.Nodes {
		if n.ModifiedIndex > maxIndex {
			maxIndex = n.ModifiedIndex
		}
	}

	originVal := resp.Node.Value

	log.Infof("convert event maxIndex: %d, %+v, %+v", maxIndex, resp, resp.Node)

	ch := make(chan zk.Event, 100)

	go func(index uint64) {
		conn, err := e.pool.Get()
		if err != nil {
			return
		}

		defer e.pool.Put(conn)
		c := conn.(*PooledEtcdClient).c

		for {
			index++
			resp, err := c.Watch(key, index, children, nil, nil)
			if err != nil {
				if ec, ok := err.(*etcd.EtcdError); ok {
					if ec.ErrorCode == etcderr.EcodeEventIndexCleared {
						index++
						continue
					}
				}

				log.Warning("watch", err)
				ch <- convertToZkEvent(resp, err)
				return
			}

			if originVal == string(resp.Node.Value) { //keep alive event
				continue
			}

			log.Infof("got event %+v, %+v", resp, resp.Node)

			ch <- convertToZkEvent(resp, err)
		}
	}(maxIndex)

	return resp, nil, ch, nil
}

func (e *etcdImpl) GetW(key string) (data []byte, stat zk.Stat, watch <-chan zk.Event, err error) {
	resp, stat, watch, err := e.watch(key, false)
	if err != nil {
		return
	}

	return []byte(resp.Node.Value), stat, watch, nil
}

func (e *etcdImpl) Children(key string) (children []string, stat zk.Stat, err error) {
	conn, err := e.pool.Get()
	if err != nil {
		return nil, nil, err
	}

	defer e.pool.Put(conn)
	c := conn.(*PooledEtcdClient).c

	log.Debug("Children", key)
	resp, err := c.Get(key, true, false)
	if resp == nil {
		return nil, nil, convertToZkError(err)
	}

	for _, c := range resp.Node.Nodes {
		children = append(children, path.Base(c.Key))
	}

	return
}

func (e *etcdImpl) ChildrenW(key string) (children []string, stat zk.Stat, watch <-chan zk.Event, err error) {
	resp, stat, watch, err := e.watch(key, true)
	if err != nil {
		return nil, stat, nil, convertToZkError(err)
	}

	for _, c := range resp.Node.Nodes {
		children = append(children, path.Base(c.Key))
	}

	return
}

func (e *etcdImpl) Exists(key string) (exist bool, stat zk.Stat, err error) {
	conn, err := e.pool.Get()
	if err != nil {
		return false, nil, err
	}

	defer e.pool.Put(conn)
	c := conn.(*PooledEtcdClient).c

	_, err = c.Get(key, true, false)
	if err == nil {
		return true, nil, nil
	}

	if ec, ok := err.(*etcd.EtcdError); ok {
		if ec.ErrorCode == etcderr.EcodeKeyNotFound {
			return false, nil, nil
		}
	}

	return false, nil, convertToZkError(err)
}

func (e *etcdImpl) ExistsW(key string) (exist bool, stat zk.Stat, watch <-chan zk.Event, err error) {
	_, stat, watch, err = e.watch(key, false)
	if err != nil {
		return false, nil, nil, convertToZkError(err)
	}

	return true, nil, watch, nil
}

const MAX_TTL = 365 * 24 * 60 * 60

func (e *etcdImpl) doKeepAlive(key string, ttl uint64) error {
	time.Sleep(1 * time.Second)
	conn, err := e.pool.Get()
	if err != nil {
		return err
	}

	defer e.pool.Put(conn)
	c := conn.(*PooledEtcdClient).c

	resp, err := c.Get(key, false, false)
	if err != nil {
		log.Error(err)
		return err
	}

	if resp.Node.Dir {
		log.Error("can not set ttl to directory", key)
		return err
	}

	resp, err = c.Set(key, resp.Node.Value, ttl)
	if resp == nil {
		log.Error(err)
		return err
	}

	return nil
}

//todo:add test for keepAlive
func (e *etcdImpl) keepAlive(key string, ttl uint64) {
	go func() {
		for {
			err := e.doKeepAlive(key, ttl)
			if err != nil {
				log.Error(err)
				return
			}
		}
	}()
}

func (e *etcdImpl) Create(wholekey string, value []byte, flags int32, aclv []zk.ACL) (keyCreated string, err error) {
	seq := (flags & zk.FlagSequence) != 0
	tmp := (flags & zk.FlagEphemeral) != 0
	ttl := uint64(MAX_TTL)
	if tmp {
		ttl = 5
	}

	var resp *etcd.Response

	conn, err := e.pool.Get()
	if err != nil {
		return "", err
	}

	defer e.pool.Put(conn)
	c := conn.(*PooledEtcdClient).c

	fn := c.Create
	log.Info("create", wholekey)

	if seq {
		wholekey = path.Dir(wholekey)
		fn = c.CreateInOrder
	} else {
		for _, v := range aclv {
			if v.Perms == PERM_DIRECTORY {
				log.Info("etcdImpl:create directory", wholekey)
				fn = nil
				resp, err = c.CreateDir(wholekey, uint64(ttl))
				if err != nil {
					return "", convertToZkError(err)
				}
			}
		}
	}

	if fn == nil {
		if tmp {
			e.keepAlive(wholekey, ttl)
		}
		return resp.Node.Key, nil
	}

	resp, err = fn(wholekey, string(value), uint64(ttl))
	if err != nil {
		return "", convertToZkError(err)
	}

	if tmp {
		e.keepAlive(resp.Node.Key, ttl)
	}

	return resp.Node.Key, nil
}

func (e *etcdImpl) Set(key string, value []byte, version int32) (stat zk.Stat, err error) {
	if version == 0 {
		return nil, errors.New("invalid version")
	}

	conn, err := e.pool.Get()
	if err != nil {
		return nil, err
	}

	defer e.pool.Put(conn)
	c := conn.(*PooledEtcdClient).c

	resp, err := c.Get(key, true, false)
	if resp == nil {
		return nil, convertToZkError(err)
	}

	_, err = c.Set(key, string(value), uint64(resp.Node.TTL))
	return nil, convertToZkError(err)
}

func (e *etcdImpl) Delete(key string, version int32) (err error) {
	//todo: handle version
	conn, err := e.pool.Get()
	if err != nil {
		return err
	}

	defer e.pool.Put(conn)
	c := conn.(*PooledEtcdClient).c

	resp, err := c.Get(key, true, false)
	if resp == nil {
		return convertToZkError(err)
	}

	if resp.Node.Dir {
		_, err = c.DeleteDir(key)
	} else {
		_, err = c.Delete(key, false)
	}

	return convertToZkError(err)
}

func (e *etcdImpl) GetACL(key string) ([]zk.ACL, zk.Stat, error) {
	return nil, nil, nil
}

func (e *etcdImpl) SetACL(key string, aclv []zk.ACL, version int32) (zk.Stat, error) {
	return nil, nil
}

func (e *etcdImpl) Close() {
	//how to implement this
}
