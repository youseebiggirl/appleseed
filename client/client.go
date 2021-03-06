package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"sync"

	"github.com/YOUSEEBIGGIRL/appleseed/codec"
	"github.com/YOUSEEBIGGIRL/appleseed/loadbalance"
	"github.com/YOUSEEBIGGIRL/appleseed/registry"
)

func GetServerAddr(ctx context.Context, reg registry.Client, lb loadbalance.Balancer, serviceName string) (addr string, err error) {
	// 从注册中心中获取 serviceName 的所有地址
	addrs, err := reg.Get(ctx, serviceName)
	if err != nil {
		return
	}
	if len(addrs) == 0 {
		return "", fmt.Errorf("this service[%v] no address", serviceName)
	}

	for _, addr := range addrs {
		lb.Add(addr)
	}
	// 通过负载均衡选择其中的一个
	addr = lb.Get()
	return
}

type Client struct {
	//reqMu     sync.Mutex // 似乎没什么用，一把锁足以
	codec      codec.ClientCodec
	request    codec.RequestHeader
	mu         sync.Mutex       // 保护 pending
	globalSeq  uint64           // 为 request 分配 seq
	pending    map[uint64]*Call // 保存所有请求，请求完成后，会进行移除
	serverAddr string           // 当前调用的服务的地址，如果 watch 到该地址下线或者变更，可以进行相应的处理
	closing    bool             // user has called Close
	shutdown   bool             // server has told us to stop
}

func NewClient(conn io.ReadWriteCloser, serverAddr string) *Client {
	cc := codec.NewGobClientCodec(conn)
	c := newClientWithCodec(cc)
	c.serverAddr = serverAddr
	return c
}

func newClientWithCodec(codec codec.ClientCodec) *Client {
	cli := &Client{
		codec:   codec,
		pending: make(map[uint64]*Call),
	}
	go cli.recv()
	return cli
}

type Call struct {
	ServiceMethod string
	Args          any
	Reply         any
	Error         error
	Done          chan *Call
}

func (c *Call) done() {
	select {
	case c.Done <- c:
	default:
		// 队列已满，请求被丢弃
		log.Println("rpc: client chan capacity is full, this call will be discard")
	}
}

func (c *Client) send(call *Call) {
	//c.reqMu.Lock()
	//defer c.reqMu.Unlock()

	c.mu.Lock()
	seq := c.globalSeq
	c.globalSeq++
	c.pending[seq] = call
	c.mu.Unlock()

	c.request.Seq = seq
	c.request.ServiceMethod = call.ServiceMethod
	if err := c.codec.WriteRequest(&c.request, call.Args); err != nil {
		c.mu.Lock()
		call := c.pending[seq]
		delete(c.pending, seq)
		c.mu.Unlock()

		if call != nil {
			call.Error = err
			call.done()
		}
	}
}

func (c *Client) recv() {
	var resp codec.ResponseHeader
	var err error
	for err == nil {
		if err = c.codec.ReadResponseHeader(&resp); err != nil {
			log.Println("read response header error: ", err)
			break
		}
		seq := resp.Seq
		c.mu.Lock()
		// 从 pending 中获取对应（seq 相同）的 call，并移除
		call := c.pending[seq]
		delete(c.pending, seq)
		c.mu.Unlock()

		switch {
		// 源码里对这一情况也进行了判断，但是注释用机翻完全看不懂，seq 既然是从 response
		// 中获取的，那么怎么可能在 pending 中找不到呢？
		case call == nil:
		case resp.Error != "":
			call.Error = errors.New(resp.Error)
			// 虽然发生了错误，但是仍然需要将连接中的剩余数据（body）消费掉
			// 如果 gob.Decode() 传入的是 nil，那么 gob 会读取连接中的一个值并
			// 将该值丢弃，比如 conn 中使用 gob 序列化了 a，b 两个对象，此时
			// 第一次 decode(nil)，那么 gob 将从 conn 中读取 a 并将其丢弃，
			// 第二次 decode(&b)，gob 会读取下一个值 b
			if err := c.codec.ReadResponseBody(nil); err != nil {
				call.Error = err
			}
			call.done()
		default:
			if err := c.codec.ReadResponseBody(call.Reply); err != nil {
				call.Error = err
			}
			call.done()
		}
	}
	// 如果流程走到这里，说明发生了 err
	c.mu.Lock()
	c.shutdown = true
	// 连接中没有数据可读了，这种情况可能是服务端已经下线了
	if err == io.EOF {

	}
	// 通知所有剩余的 call 发生了错误
	for _, call := range c.pending {
		call.Error = err
		call.done()
	}
}

func (c *Client) Go(ctx context.Context, serviceMethod string, arg, reply any, done chan *Call) *Call {
	call := new(Call)
	call.ServiceMethod = serviceMethod
	call.Args = arg
	call.Reply = reply
	if done == nil {
		done = make(chan *Call, 10)
	} else {
		if cap(done) == 0 {
			log.Panic("rpc: done channel is unbuffered")
		}
	}
	call.Done = done

	select {
	case <-ctx.Done():
		log.Println("time out")
		call.Error = errors.New("rpc call error: time out")
		call.done()
		return call
	default:
	}

	c.send(call)
	return call
}

func (c *Client) Call(ctx context.Context, serviceMethod string, arg, reply any) error {
	call := <-c.Go(ctx, serviceMethod, arg, reply, make(chan *Call, 1)).Done
	return call.Error
}
