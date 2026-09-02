package dexos

import "encoding/binary"

// ── 内核命令的规范编码 ──
//
// 本文件是 SDK 唯一必须逐字节正确的部分。
//
// 这是整个 SDK 的技术核心,也是唯一必须逐字节正确的部分:agent 代执行的签名承诺的是
//
//	commandHash = keccak256(规范编码字节)
//
// 内核收到请求后会用**自己的**编码器重新编码内层命令再算哈希比对。编码差一个字节,
// 签名就对不上,请求被拒 —— 不会静默出错,但也无从调试。所以这里的每个字段顺序、
// 每个整数宽度、字节序都必须与 crates/kernel/src/codec.rs 完全一致。
//
// 三条容易踩的:
//   - 多字节整数一律**小端**(内核用 to_le_bytes),不是网络序
//   - Side:Buy=0 Sell=1;TimeInForce:Gtc=0 Ioc=1 PostOnly=2 Fok=3
//   - OrderId 是打包值:(market << 48) | seq,编码时按 u64 整体写出
//
// 命令 tag 是 append-only 的:内核那边只允许追加、禁止重排复用,所以这里的常量
// 一旦发布就不能改。testdata/golden.json 是与 Rust 对拍的金样字节,codec_test.go
// 逐条比对 —— 内核改了编码而这里没跟上时,那组测试会红。
// codecVersion 是规范编码的**第一个字节**,写在 tag 之前(内核 encode_command 首行
// 就是 put_u8(COMMAND_CODEC_VERSION))。漏掉它整条编码就整体错位一字节 ——
// 这正是金样对拍抓出来的第一个 bug,靠比对结构体定义永远发现不了。
const codecVersion byte = 0x01

const (
	tagCancelOrder  byte = 0x06
	tagBatchCancel  byte = 0x0F
	tagBatchPlace   byte = 0x31
	tagBatchReplace byte = 0x3E
)

// Side 买卖方向。取值必须与内核的 put_side 一致。
type Side uint8

const (
	Buy  Side = 0
	Sell Side = 1
)

func (s Side) String() string {
	if s == Sell {
		return "sell"
	}
	return "buy"
}

// TimeInForce 订单有效期类型。取值必须与内核的 put_tif 一致。
type TimeInForce uint8

const (
	GTC      TimeInForce = 0
	IOC      TimeInForce = 1
	PostOnly TimeInForce = 2
	FOK      TimeInForce = 3
)

// OrderID 是打包值 (market << 48) | seq —— 与内核 OrderId::new 一致。
type OrderID uint64

// NewOrderID 按内核的打包规则构造。seq 必须 < 2^48。
func NewOrderID(market uint16, seq uint64) OrderID {
	return OrderID(uint64(market)<<48 | (seq & (1<<48 - 1)))
}

// Market 返回打包值里的市场号。
func (o OrderID) Market() uint16 { return uint16(uint64(o) >> 48) }

// Seq 返回打包值里的订单序号。
func (o OrderID) Seq() uint64 { return uint64(o) & (1<<48 - 1) }

// BatchOrder 批量下单的单条。字段顺序即编码顺序。
type BatchOrder struct {
	Market     uint16
	Side       Side
	Price      uint32
	Lots       uint64
	TIF        TimeInForce
	ReduceOnly bool
}

// ── 编码原语(全部小端,与内核 put_* 对齐)──

type encoder struct{ buf []byte }

func (e *encoder) u8(v byte) { e.buf = append(e.buf, v) }
func (e *encoder) u16(v uint16) {
	var b [2]byte
	binary.LittleEndian.PutUint16(b[:], v)
	e.buf = append(e.buf, b[:]...)
}
func (e *encoder) u32(v uint32) {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], v)
	e.buf = append(e.buf, b[:]...)
}
func (e *encoder) u64(v uint64) {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], v)
	e.buf = append(e.buf, b[:]...)
}
func (e *encoder) boolean(v bool) {
	if v {
		e.u8(1)
	} else {
		e.u8(0)
	}
}
func (e *encoder) order(o BatchOrder) {
	e.u16(o.Market)
	e.u8(byte(o.Side))
	e.u32(o.Price)
	e.u64(o.Lots)
	e.u8(byte(o.TIF))
	e.boolean(o.ReduceOnly)
}

// EncodeBatchPlace 规范编码 Command::BatchPlace。
//
// 布局:tag | account u32 | nowMs u64 | len u32 | 每条订单
// 注意 nowMs 在长度前缀**之前** —— 与 BatchCancel 的布局不同,别照着抄。
func EncodeBatchPlace(account uint32, nowMs uint64, orders []BatchOrder) []byte {
	e := &encoder{}
	e.u8(codecVersion)
	e.u8(tagBatchPlace)
	e.u32(account)
	e.u64(nowMs)
	e.u32(uint32(len(orders)))
	for _, o := range orders {
		e.order(o)
	}
	return e.buf
}

// EncodeBatchCancel 规范编码 Command::BatchCancel。
//
// 布局:tag | account u32 | len u32 | 每个 OrderId u64
// 这条**没有 nowMs** —— 撤单不需要时间语义。
func EncodeBatchCancel(account uint32, orders []OrderID) []byte {
	e := &encoder{}
	e.u8(codecVersion)
	e.u8(tagBatchCancel)
	e.u32(account)
	e.u32(uint32(len(orders)))
	for _, o := range orders {
		e.u64(uint64(o))
	}
	return e.buf
}

// EncodeBatchReplace 规范编码 Command::BatchReplace(原子换单:先撤后挂)。
//
// 布局:tag | account u32 | nowMs u64 | cancels len u32 | 每个 OrderId u64
//
//	| orders len u32 | 每条订单
//
// 两个变长段挨在一起,长度前缀写错顺序是这里最容易犯的错 ——
// 内核侧的 roundtrip 测试专门有一条空/空的边界用例钉这个。
func EncodeBatchReplace(account uint32, nowMs uint64, cancels []OrderID, orders []BatchOrder) []byte {
	e := &encoder{}
	e.u8(codecVersion)
	e.u8(tagBatchReplace)
	e.u32(account)
	e.u64(nowMs)
	e.u32(uint32(len(cancels)))
	for _, c := range cancels {
		e.u64(uint64(c))
	}
	e.u32(uint32(len(orders)))
	for _, o := range orders {
		e.order(o)
	}
	return e.buf
}

// EncodeCancelOrder 规范编码 Command::CancelOrder(单撤)。
//
// 布局:tag | account u32 | OrderId u64
func EncodeCancelOrder(account uint32, order OrderID) []byte {
	e := &encoder{}
	e.u8(codecVersion)
	e.u8(tagCancelOrder)
	e.u32(account)
	e.u64(uint64(order))
	return e.buf
}
