package types

import (
	"github.com/filecoin-project/go-lotus/chain/address"
	cbor "github.com/ipfs/go-ipld-cbor"
)

func init() {
	cbor.RegisterCborType(SignedStorageAsk{})
	cbor.RegisterCborType(StorageAsk{})
}

type SignedStorageAsk struct {
	Ask       *StorageAsk
	Signature *Signature
}

type StorageAsk struct {
	Price        BigInt
	MinPieceSize uint64
	Miner        address.Address
	Timestamp    int64
	Expiry       int64
	SeqNo        uint64
}
