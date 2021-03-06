package jail

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/robertkrimen/otto"
	"github.com/status-im/status-go/geth/common"
	"github.com/status-im/status-go/geth/jail/internal/vm"
	"github.com/status-im/status-go/geth/log"
	"github.com/status-im/status-go/static"
)

// FIXME(tiabc): Get rid of this global variable. Move it to a constructor or initialization.
var web3JSCode = static.MustAsset("scripts/web3.js")

// errors
var (
	ErrInvalidJail = errors.New("jail environment is not properly initialized")
)

// Jail represents jailed environment inside of which we hold multiple cells.
// Each cell is a separate JavaScript VM.
type Jail struct {
	// FIXME(tiabc): This mutex handles cells field access and must be renamed appropriately: cellsMutex
	sync.RWMutex
	nodeManager    common.NodeManager
	accountManager common.AccountManager
	txQueueManager common.TxQueueManager
	policy         *ExecutionPolicy
	cells          map[string]*Cell // jail supports running many isolated instances of jailed runtime
	baseJSCode     string           // JavaScript used to initialize all new cells with
}

// New returns new Jail environment with the associated NodeManager and
// AccountManager.
func New(
	nodeManager common.NodeManager, accountManager common.AccountManager, txQueueManager common.TxQueueManager,
) *Jail {
	if nodeManager == nil || accountManager == nil || txQueueManager == nil {
		panic("Jail is missing mandatory dependencies")
	}
	return &Jail{
		nodeManager:    nodeManager,
		accountManager: accountManager,
		txQueueManager: txQueueManager,
		cells:          make(map[string]*Cell),
		policy:         NewExecutionPolicy(nodeManager, accountManager, txQueueManager),
	}
}

// BaseJS allows to setup initial JavaScript to be loaded on each jail.Parse().
func (jail *Jail) BaseJS(js string) {
	jail.baseJSCode = js
}

// NewCell initializes and returns a new jail cell.
func (jail *Jail) NewCell(chatID string) (common.JailCell, error) {
	if jail == nil {
		return nil, ErrInvalidJail
	}

	vm := otto.New()

	cell, err := newCell(chatID, vm)
	if err != nil {
		return nil, err
	}

	jail.Lock()
	jail.cells[chatID] = cell
	jail.Unlock()

	return cell, nil
}

// Cell returns the existing instance of Cell.
func (jail *Jail) Cell(chatID string) (common.JailCell, error) {
	jail.RLock()
	defer jail.RUnlock()

	cell, ok := jail.cells[chatID]
	if !ok {
		return nil, fmt.Errorf("cell[%s] doesn't exist", chatID)
	}

	return cell, nil
}

// Parse creates a new jail cell context, with the given chatID as identifier.
// New context executes provided JavaScript code, right after the initialization.
func (jail *Jail) Parse(chatID, js string) string {
	if jail == nil {
		return makeError(ErrInvalidJail.Error())
	}

	cell, err := jail.Cell(chatID)
	if err != nil {
		if _, mkerr := jail.NewCell(chatID); mkerr != nil {
			return makeError(mkerr.Error())
		}

		cell, _ = jail.Cell(chatID)
	}

	// init jeth and its handlers
	if err = cell.Set("jeth", struct{}{}); err != nil {
		return makeError(err.Error())
	}

	if err = registerHandlers(jail, cell, chatID); err != nil {
		return makeError(err.Error())
	}

	initJs := jail.baseJSCode + ";"
	if _, err = cell.Run(initJs); err != nil {
		return makeError(err.Error())
	}

	jjs := string(web3JSCode) + `
	var Web3 = require('web3');
	var web3 = new Web3(jeth);
	var Bignumber = require("bignumber.js");
        function bn(val){
            return new Bignumber(val);
        }
	` + js + "; var catalog = JSON.stringify(_status_catalog);"
	if _, err = cell.Run(jjs); err != nil {
		return makeError(err.Error())
	}

	res, err := cell.Get("catalog")
	if err != nil {
		return makeError(err.Error())
	}

	return makeResult(res.String(), err)
}

// Call executes the `call` function w/i a jail cell context identified by the chatID.
func (jail *Jail) Call(chatID, this, args string) string {
	cell, err := jail.Cell(chatID)
	if err != nil {
		return makeError(err.Error())
	}

	res, err := cell.Call("call", nil, this, args)

	return makeResult(res.String(), err)
}

// Send will serialize the first argument, send it to the node and returns the response.
// IMPORTANT: Don't use `call.Otto` in this function unless you want to run into race conditions. Use `vm` instead.
// nolint: errcheck, unparam
func (jail *Jail) Send(call otto.FunctionCall, vm *vm.VM) otto.Value {
	reqVal, err := vm.Call("JSON.stringify", nil, call.Argument(0))
	if err != nil {
		throwJSException(err)
	}

	var (
		rawReq = []byte(reqVal.String())
		reqs   []common.RPCCall
		batch  bool
	)

	if rawReq[0] == '[' {
		batch = true
		err = json.Unmarshal(rawReq, &reqs)
	} else {
		batch = false
		reqs = make([]common.RPCCall, 1)
		err = json.Unmarshal(rawReq, &reqs[0])
	}
	if err != nil {
		throwJSException(fmt.Errorf("can't unmarshal %v (batch=%v): %s", string(rawReq), batch, err))
	}

	resps, err := vm.Call("new Array", nil)
	if err != nil {
		throwJSException(fmt.Errorf("can't create Array: %s", err))
	}

	// Execute the requests.
	for _, req := range reqs {
		log.Info("execute request", "method", req.Method)
		res, err := jail.policy.Execute(req, vm)
		if err != nil {
			log.Info("request errored", "error", err.Error())
			switch err.(type) {
			case common.StopRPCCallError:
				return newErrorResponseOtto(vm, err.Error(), nil)
			default:
				res = newErrorResponse(err.Error(), &req.ID)
			}
		}

		_, err = resps.Object().Call("push", res)
		if err != nil {
			throwJSException(fmt.Errorf("can't push result: %s", err))
		}
	}

	// Return the responses either to the callback (if supplied)
	// or directly as the return value.
	if batch {
		return resps
	}
	v, err := resps.Object().Get("0")
	if err != nil {
		throwJSException(err)
	}
	return v
}

func newErrorResponse(msg string, id interface{}) map[string]interface{} {
	// Bundle the error into a JSON RPC call response
	return map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      id,
		"error": map[string]interface{}{
			"code":    -32603, // Internal JSON-RPC Error, see http://www.jsonrpc.org/specification#error_object
			"message": msg,
		},
	}
}

func newErrorResponseOtto(vm *vm.VM, msg string, id interface{}) otto.Value {
	// TODO(tiabc): Handle errors.
	errResp, _ := json.Marshal(newErrorResponse(msg, id))
	errRespVal, _ := vm.Run("(" + string(errResp) + ")")
	return errRespVal
}

func newResultResponse(vm *otto.Otto, result interface{}) otto.Value {
	resp, _ := vm.Object(`({"jsonrpc":"2.0"})`)
	resp.Set("result", result) // nolint: errcheck

	return resp.Value()
}

// throwJSException panics on an otto.Value. The Otto VM will recover from the
// Go panic and throw msg as a JavaScript error.
func throwJSException(msg error) otto.Value {
	val, err := otto.ToValue(msg.Error())
	if err != nil {
		log.Error(fmt.Sprintf("Failed to serialize JavaScript exception %v: %v", msg.Error(), err))
	}
	panic(val)
}

// JSONError is wrapper around errors, that are sent upwards
type JSONError struct {
	Error string `json:"error"`
}

func makeError(error string) string {
	str := JSONError{
		Error: error,
	}
	outBytes, _ := json.Marshal(&str)
	return string(outBytes)
}

func makeResult(res string, err error) string {
	var out string
	if err != nil {
		out = makeError(err.Error())
	} else {
		if "undefined" == res {
			res = "null"
		}
		out = fmt.Sprintf(`{"result": %s}`, res)
	}

	return out
}
