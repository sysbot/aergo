/**
 *  @file
 *  @copyright defined in aergo/LICENSE.txt
 */

package contract

/*
#cgo CFLAGS: -I${SRCDIR}/../libtool/include/luajit-2.0
#cgo LDFLAGS: ${SRCDIR}/../libtool/lib/libluajit-5.1.a -lm

#include <stdlib.h>
#include <string.h>
#include "vm.h"
*/
import "C"
import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"unsafe"

	"github.com/aergoio/aergo-lib/db"
	"github.com/aergoio/aergo-lib/log"
	"github.com/aergoio/aergo/types"
	"github.com/mr-tron/base58/base58"
)

const DbName = "contracts.db"

var (
	ctrLog *log.Logger
	DB     db.DB
)

type Contract struct {
	code    []byte
	address []byte
}

type LState = C.struct_lua_State
type LBlockchainCtx = C.struct_blockchain_ctx

type Executor struct {
	L             *LState
	contract      *Contract
	err           error
	blockchainCtx *LBlockchainCtx
	jsonRet       string
}

func init() {
	ctrLog = log.NewLogger("contract")
}

func NewContext(Sender, blockHash, txHash []byte, blockHeight uint64,
	timestamp int64, node string, confirmed bool, contractID []byte) *LBlockchainCtx {

	var iConfirmed int
	if confirmed {
		iConfirmed = 1
	}

	return &LBlockchainCtx{
		sender:      C.CString(base58.Encode(Sender)),
		blockHash:   C.CString(hex.EncodeToString(blockHash)),
		txHash:      C.CString(hex.EncodeToString(txHash)),
		blockHeight: C.ulonglong(blockHeight),
		timestamp:   C.longlong(timestamp),
		node:        C.CString(node),
		confirmed:   C.int(iConfirmed),
		contractId:  C.CString(base58.Encode(contractID)),
	}
}

func newLState() *LState {
	return C.vm_newstate()
}

func (L *LState) Close() {
	if L != nil {
		C.lua_close(L)
	}
}

func newExecutor(contract *Contract, bcCtx *LBlockchainCtx) *Executor {
	ce := &Executor{
		contract: contract,
		L:        newLState(),
	}
	if cErrMsg := C.vm_loadbuff(
		ce.L,
		(*C.char)(unsafe.Pointer(&contract.code[0])),
		C.size_t(len(contract.code)),
		C.CString(base58.Encode(contract.address)),
		bcCtx,
	); cErrMsg != nil {
		errMsg := C.GoString(cErrMsg)
		C.free(unsafe.Pointer(cErrMsg))
		ctrLog.Error().Str("error", errMsg)
		ce.err = errors.New(errMsg)
	}
	return ce
}

func (ce *Executor) call(abi *types.ABI) {
	if ce.err != nil {
		return
	}
	C.vm_getfield(ce.L, C.CString("abi"))
	C.lua_getfield(ce.L, -1, C.CString("call"))
	C.lua_pushstring(ce.L, C.CString(abi.Name))
	for _, v := range abi.Args {
		switch arg := v.(type) {
		case string:
			C.lua_pushstring(ce.L, C.CString(arg))
		case int:
			C.lua_pushinteger(ce.L, C.long(arg))
		case bool:
			var b int
			if arg {
				b = 1
			}
			C.lua_pushboolean(ce.L, C.int(b))
		default:
			ce.err = errors.New("unsupported type")
			return
		}
	}
	nret := C.int(0)
	if cErrMsg := C.vm_pcall(ce.L, C.int(len(abi.Args)+1), &nret); cErrMsg != nil {
		errMsg := C.GoString(cErrMsg)
		C.free(unsafe.Pointer(cErrMsg))
		ctrLog.Warn().Str("error", errMsg).Msgf("contract %s", base58.Encode(ce.contract.address))
		ce.err = errors.New(errMsg)
		return
	}
	ce.jsonRet = C.GoString(C.vm_get_json_ret(ce.L, nret))
}

func (ce *Executor) close() {
	if ce != nil {
		ce.L.Close()
		if ce.blockchainCtx != nil {
			context := ce.blockchainCtx
			C.free(unsafe.Pointer(context.sender))
			C.free(unsafe.Pointer(context.blockHash))
			C.free(unsafe.Pointer(context.txHash))
			C.free(unsafe.Pointer(context.node))
			C.free(unsafe.Pointer(context))
		}
	}
}

func Call(code, contractAddress, txHash []byte, bcCtx *LBlockchainCtx) error {
	var err error
	contract := getContract(contractAddress)
	if contract == nil {
		err = fmt.Errorf("cannot find contract %s", string(contractAddress))
		ctrLog.Warn().AnErr("err", err)
	}
	var abi types.ABI
	err = json.Unmarshal(code, &abi)
	if err != nil {
		ctrLog.Warn().AnErr("error", err).Msgf("contract %s", base58.Encode(contractAddress))
	}
	var ce *Executor
	defer ce.close()
	if err == nil {
		ctrLog.Debug().Str("abi", string(code)).Msgf("contract %s", base58.Encode(contractAddress))
		ce = newExecutor(contract, bcCtx)
		ce.call(&abi)
		err = ce.err
	}
	receipt := types.NewReceipt(contractAddress, "SUCCESS", ce.jsonRet)
	if err != nil {
		receipt.Status = err.Error()
	}
	DB.Set(txHash, receipt.Bytes())
	return err
}

func Create(code, contractAddress, txHash []byte) error {
	ctrLog.Debug().Str("contractAddress", base58.Encode(contractAddress)).Msg("new contract is deployed")
	DB.Set(contractAddress, code)
	receipt := types.NewReceipt(contractAddress, "CREATED", "{}")
	DB.Set(txHash, receipt.Bytes())
	return nil
}

func getContract(contractAddress []byte) *Contract {
	val := DB.Get(contractAddress)
	if len(val) > 0 {
		return &Contract{
			code:    val,
			address: contractAddress[:],
		}
	}
	return nil
}

func GetReceipt(txHash []byte) *types.Receipt {
	val := DB.Get(txHash)
	if len(val) == 0 {
		return nil
	}
	return types.NewReceiptFromBytes(val)
}

//export LuaSetDB
func LuaSetDB(key *C.char, value *C.char) {
	keyString := C.GoString(key)
	valueString := C.GoString(value)

	DB.Set([]byte(keyString), []byte(valueString))
}

//export LuaGetDB
func LuaGetDB(key *C.char) unsafe.Pointer {
	keyString := C.GoString(key)

	return C.CBytes(DB.Get([]byte(keyString)))
}

//export LuaDelDB
func LuaDelDB(key *C.char) {
	keyString := C.GoString(key)

	DB.Delete([]byte(keyString))
}

