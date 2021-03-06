/*
Copyright SecureKey Technologies Inc. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package dispatcher

import (
	"math"
	"reflect"
	"regexp"
	"sync/atomic"
	"time"

	"github.com/golang/protobuf/proto"
	"github.com/hyperledger/fabric-sdk-go/pkg/context/api/fab"
	"github.com/hyperledger/fabric-sdk-go/pkg/logging"
	"github.com/hyperledger/fabric-sdk-go/pkg/options"
	ledgerutil "github.com/hyperledger/fabric-sdk-go/third_party/github.com/hyperledger/fabric/core/ledger/util"
	cb "github.com/hyperledger/fabric-sdk-go/third_party/github.com/hyperledger/fabric/protos/common"
	pb "github.com/hyperledger/fabric-sdk-go/third_party/github.com/hyperledger/fabric/protos/peer"
	utils "github.com/hyperledger/fabric-sdk-go/third_party/github.com/hyperledger/fabric/protos/utils"
	"github.com/pkg/errors"
)

var logger = logging.NewLogger("fabric_sdk_go")

const (
	dispatcherStateInitial = iota
	dispatcherStateStarted
	dispatcherStateStopped
)

// Handler is the handler for a given event type.
type Handler func(Event)

// HandlerRegistry contains the handlers for each type of event
type HandlerRegistry map[reflect.Type]Handler

// Dispatcher is responsible for handling all events, including connection and registration events originating from the client,
// and events originating from the channel event service. All events are processed in a single Go routine
// in order to avoid any race conditions and to ensure that events are processed in the order in which they are received.
// This also avoids the need for synchronization.
type Dispatcher struct {
	params
	handlers                   map[reflect.Type]Handler
	eventch                    chan interface{}
	blockRegistrations         []*BlockReg
	filteredBlockRegistrations []*FilteredBlockReg
	txRegistrations            map[string]*TxStatusReg
	ccRegistrations            map[string]*ChaincodeReg
	state                      int32
	lastBlockNum               uint64
}

// New creates a new Dispatcher.
func New(opts ...options.Opt) *Dispatcher {
	logger.Debugf("Creating new dispatcher.")

	params := defaultParams()
	options.Apply(params, opts)

	return &Dispatcher{
		params:          *params,
		handlers:        make(map[reflect.Type]Handler),
		eventch:         make(chan interface{}, params.eventConsumerBufferSize),
		txRegistrations: make(map[string]*TxStatusReg),
		ccRegistrations: make(map[string]*ChaincodeReg),
		state:           dispatcherStateInitial,
		lastBlockNum:    math.MaxUint64,
	}
}

// RegisterHandlers registers all of the handlers by event type
func (ed *Dispatcher) RegisterHandlers() {
	ed.RegisterHandler(&RegisterChaincodeEvent{}, ed.handleRegisterCCEvent)
	ed.RegisterHandler(&RegisterTxStatusEvent{}, ed.handleRegisterTxStatusEvent)
	ed.RegisterHandler(&RegisterBlockEvent{}, ed.handleRegisterBlockEvent)
	ed.RegisterHandler(&RegisterFilteredBlockEvent{}, ed.handleRegisterFilteredBlockEvent)
	ed.RegisterHandler(&UnregisterEvent{}, ed.handleUnregisterEvent)
	ed.RegisterHandler(&StopEvent{}, ed.HandleStopEvent)
	ed.RegisterHandler(&cb.Block{}, ed.handleBlockEvent)
	ed.RegisterHandler(&pb.FilteredBlock{}, ed.handleFilteredBlockEvent)
}

// EventCh returns the channel to which events may be posted
func (ed *Dispatcher) EventCh() (chan<- interface{}, error) {
	state := ed.getState()
	if state == dispatcherStateStarted {
		return ed.eventch, nil
	}
	return nil, errors.Errorf("dispatcher not started - Current state [%d]", state)
}

// Start starts dispatching events as they arrive. All events are processed in
// a single Go routine in order to avoid any race conditions
func (ed *Dispatcher) Start() error {
	if !ed.setState(dispatcherStateInitial, dispatcherStateStarted) {
		return errors.New("cannot start dispatcher since it's not in its initial state")
	}

	ed.RegisterHandlers()

	go func() {
		for {
			if ed.getState() == dispatcherStateStopped {
				break
			}

			logger.Debug("Listening for events...")
			e, ok := <-ed.eventch
			if !ok {
				break
			}

			logger.Debugf("Received event: %v", reflect.TypeOf(e))

			if handler, ok := ed.handlers[reflect.TypeOf(e)]; ok {
				logger.Debugf("Dispatching event: %v", reflect.TypeOf(e))
				handler(e)
			} else {
				logger.Errorf("Handler not found for: %s", reflect.TypeOf(e))
			}
		}
		logger.Debug("Exiting event dispatcher")
	}()
	return nil
}

// LastBlockNum returns the block number of the last block for which an event was received.
func (ed *Dispatcher) LastBlockNum() uint64 {
	return atomic.LoadUint64(&ed.lastBlockNum)
}

// updateLastBlockNum updates the value of lastBlockNum and
// returns the updated value.
func (ed *Dispatcher) updateLastBlockNum(blockNum uint64) error {
	// The Deliver Service shouldn't be sending blocks out of order.
	// Log an error if we detect this happening.
	lastBlockNum := atomic.LoadUint64(&ed.lastBlockNum)
	if lastBlockNum == math.MaxUint64 || blockNum > lastBlockNum {
		atomic.StoreUint64(&ed.lastBlockNum, blockNum)
		return nil
	}
	return errors.Errorf("Expecting a block number greater than %d but received block number %d", lastBlockNum, lastBlockNum)
}

// clearBlockRegistrations removes all block registrations and closes the corresponding event channels.
// The listener will receive a 'closed' event to indicate that the channel has been closed.
func (ed *Dispatcher) clearBlockRegistrations() {
	for _, reg := range ed.blockRegistrations {
		close(reg.Eventch)
	}
	ed.blockRegistrations = nil
}

// clearFilteredBlockRegistrations removes all filtered block registrations and closes the corresponding event channels.
// The listener will receive a 'closed' event to indicate that the channel has been closed.
func (ed *Dispatcher) clearFilteredBlockRegistrations() {
	for _, reg := range ed.filteredBlockRegistrations {
		close(reg.Eventch)
	}
	ed.filteredBlockRegistrations = nil
}

// clearTxRegistrations removes all transaction registrations and closes the corresponding event channels.
// The listener will receive a 'closed' event to indicate that the channel has been closed.
func (ed *Dispatcher) clearTxRegistrations() {
	for _, reg := range ed.txRegistrations {
		logger.Debugf("Closing TX registration event channel for TxID [%s].", reg.TxID)
		close(reg.Eventch)
	}
	ed.txRegistrations = make(map[string]*TxStatusReg)
}

// clearChaincodeRegistrations removes all chaincode registrations and closes the corresponding event channels.
// The listener will receive a 'closed' event to indicate that the channel has been closed.
func (ed *Dispatcher) clearChaincodeRegistrations() {
	for _, reg := range ed.ccRegistrations {
		logger.Debugf("Closing chaincode registration event channel for CC ID [%s] and event filter [%s].", reg.ChaincodeID, reg.EventFilter)
		close(reg.Eventch)
	}
	ed.ccRegistrations = make(map[string]*ChaincodeReg)
}

// HandleStopEvent stops the dispatcher and unregisters all event registration.
// The Dispatcher is no longer usable.
func (ed *Dispatcher) HandleStopEvent(e Event) {
	event := e.(*StopEvent)

	logger.Debugf("Stopping dispatcher...")
	if !ed.setState(dispatcherStateStarted, dispatcherStateStopped) {
		logger.Warn("Cannot stop event dispatcher since it's already stopped.")
		return
	}

	// Remove all registrations and close the associated event channels
	// so that the client is notified that the registration has been removed
	ed.clearBlockRegistrations()
	ed.clearFilteredBlockRegistrations()
	ed.clearTxRegistrations()
	ed.clearChaincodeRegistrations()

	event.ErrCh <- nil
}

func (ed *Dispatcher) handleRegisterBlockEvent(e Event) {
	event := e.(*RegisterBlockEvent)

	ed.blockRegistrations = append(ed.blockRegistrations, event.Reg)
	event.RegCh <- event.Reg
}

func (ed *Dispatcher) handleRegisterFilteredBlockEvent(e Event) {
	event := e.(*RegisterFilteredBlockEvent)
	ed.filteredBlockRegistrations = append(ed.filteredBlockRegistrations, event.Reg)
	event.RegCh <- event.Reg
}

func (ed *Dispatcher) handleRegisterCCEvent(e Event) {
	event := e.(*RegisterChaincodeEvent)

	key := getCCKey(event.Reg.ChaincodeID, event.Reg.EventFilter)
	if _, exists := ed.ccRegistrations[key]; exists {
		event.ErrCh <- errors.Errorf("registration already exists for chaincode [%s] and event [%s]", event.Reg.ChaincodeID, event.Reg.EventFilter)
	} else {
		regExp, err := regexp.Compile(event.Reg.EventFilter)
		if err != nil {
			event.ErrCh <- errors.Wrapf(err, "error compiling regular expression for event filter [%s]", event.Reg.EventFilter)
		} else {
			event.Reg.EventRegExp = regExp
			ed.ccRegistrations[key] = event.Reg
			event.RegCh <- event.Reg
		}
	}
}

func (ed *Dispatcher) handleRegisterTxStatusEvent(e Event) {
	event := e.(*RegisterTxStatusEvent)

	if _, exists := ed.txRegistrations[event.Reg.TxID]; exists {
		event.ErrCh <- errors.Errorf("registration already exists for TX ID [%s]", event.Reg.TxID)
	} else {
		ed.txRegistrations[event.Reg.TxID] = event.Reg
		event.RegCh <- event.Reg
	}
}

func (ed *Dispatcher) handleUnregisterEvent(e Event) {
	event := e.(*UnregisterEvent)

	var err error
	switch registration := event.Reg.(type) {
	case *BlockReg:
		err = ed.unregisterBlockEvents(registration)
	case *FilteredBlockReg:
		err = ed.unregisterFilteredBlockEvents(registration)
	case *ChaincodeReg:
		err = ed.unregisterCCEvents(registration)
	case *TxStatusReg:
		err = ed.unregisterTXEvents(registration)
	default:
		err = errors.Errorf("Unsupported registration type: %v", reflect.TypeOf(registration))
	}
	if err != nil {
		logger.Warnf("Error in unregister: %s", err)
	}
}

func (ed *Dispatcher) handleBlockEvent(e Event) {
	ed.HandleBlock(e.(*cb.Block))
}

func (ed *Dispatcher) handleFilteredBlockEvent(e Event) {
	ed.HandleFilteredBlock(e.(*pb.FilteredBlock))
}

// HandleBlock handles a block event
func (ed *Dispatcher) HandleBlock(block *cb.Block) {
	logger.Debugf("Handling block event - Block #%d", block.Header.Number)

	if err := ed.updateLastBlockNum(block.Header.Number); err != nil {
		logger.Error(err.Error())
		return
	}

	ed.publishBlockEvents(block)
	ed.publishFilteredBlockEvents(toFilteredBlock(block))
}

// HandleFilteredBlock handles a filtered block event
func (ed *Dispatcher) HandleFilteredBlock(fblock *pb.FilteredBlock) {
	logger.Debugf("Handling filtered block event - Block #%d", fblock.Number)

	if err := ed.updateLastBlockNum(fblock.Number); err != nil {
		logger.Error(err.Error())
		return
	}

	logger.Debugf("Publishing filtered block event...")
	ed.publishFilteredBlockEvents(fblock)
}

func (ed *Dispatcher) unregisterBlockEvents(registration *BlockReg) error {
	for i, reg := range ed.blockRegistrations {
		if reg == registration {
			// Move the 0'th item to i and then delete the 0'th item
			ed.blockRegistrations[i] = ed.blockRegistrations[0]
			ed.blockRegistrations = ed.blockRegistrations[1:]
			close(reg.Eventch)
			return nil
		}
	}
	return errors.New("the provided registration is invalid")
}

func (ed *Dispatcher) unregisterFilteredBlockEvents(registration *FilteredBlockReg) error {
	for i, reg := range ed.filteredBlockRegistrations {
		if reg == registration {
			// Move the 0'th item to i and then delete the 0'th item
			ed.filteredBlockRegistrations[i] = ed.filteredBlockRegistrations[0]
			ed.filteredBlockRegistrations = ed.filteredBlockRegistrations[1:]
			close(reg.Eventch)
			return nil
		}
	}
	return errors.New("the provided registration is invalid")
}

func (ed *Dispatcher) unregisterCCEvents(registration *ChaincodeReg) error {
	key := getCCKey(registration.ChaincodeID, registration.EventFilter)
	reg, ok := ed.ccRegistrations[key]
	if !ok {
		return errors.New("the provided registration is invalid")
	}

	logger.Debugf("Unregistering CC event for CC ID [%s] and event filter [%s]...", registration.ChaincodeID, registration.EventFilter)
	close(reg.Eventch)
	delete(ed.ccRegistrations, key)
	return nil
}

func (ed *Dispatcher) unregisterTXEvents(registration *TxStatusReg) error {
	reg, ok := ed.txRegistrations[registration.TxID]
	if !ok {
		return errors.New("the provided registration is invalid")
	}

	logger.Debugf("Unregistering Tx Status event for TxID [%s]...", registration.TxID)
	close(reg.Eventch)
	delete(ed.txRegistrations, registration.TxID)
	return nil
}

func (ed *Dispatcher) publishBlockEvents(block *cb.Block) {
	for _, reg := range ed.blockRegistrations {
		if !reg.Filter(block) {
			logger.Debugf("Not sending block event for block #%d since it was filtered out.", block.Header.Number)
			continue
		}

		if ed.eventConsumerTimeout < 0 {
			select {
			case reg.Eventch <- &fab.BlockEvent{Block: block}:
			default:
				logger.Warnf("Unable to send to block event channel.")
			}
		} else if ed.eventConsumerTimeout == 0 {
			reg.Eventch <- &fab.BlockEvent{Block: block}
		} else {
			select {
			case reg.Eventch <- &fab.BlockEvent{Block: block}:
			case <-time.After(ed.eventConsumerTimeout):
				logger.Warnf("Timed out sending block event.")
			}
		}
	}
}

func (ed *Dispatcher) publishFilteredBlockEvents(fblock *pb.FilteredBlock) {
	if fblock == nil {
		logger.Warnf("Filtered block is nil. Event will not be published")
		return
	}

	logger.Debugf("Publishing filtered block event: %#v", fblock)

	for _, reg := range ed.filteredBlockRegistrations {
		if ed.eventConsumerTimeout < 0 {
			select {
			case reg.Eventch <- &fab.FilteredBlockEvent{FilteredBlock: fblock}:
			default:
				logger.Warnf("Unable to send to filtered block event channel.")
			}
		} else if ed.eventConsumerTimeout == 0 {
			reg.Eventch <- &fab.FilteredBlockEvent{FilteredBlock: fblock}
		} else {
			select {
			case reg.Eventch <- &fab.FilteredBlockEvent{FilteredBlock: fblock}:
			case <-time.After(ed.eventConsumerTimeout):
				logger.Warnf("Timed out sending filtered block event.")
			}
		}
	}

	for _, tx := range fblock.FilteredTx {
		ed.publishTxStatusEvents(tx)

		// Only send a chaincode event if the transaction has committed
		if tx.TxValidationCode == pb.TxValidationCode_VALID {
			txActions := tx.GetTransactionActions()
			if txActions == nil {
				continue
			}
			for _, action := range txActions.ChaincodeActions {
				if action.CcEvent != nil {
					ed.publishCCEvents(action.CcEvent)
				}
			}
		}
	}
}

func (ed *Dispatcher) publishTxStatusEvents(tx *pb.FilteredTransaction) {
	logger.Debugf("Publishing Tx Status event for TxID [%s]...", tx.Txid)
	if reg, ok := ed.txRegistrations[tx.Txid]; ok {
		logger.Debugf("Sending Tx Status event for TxID [%s] to registrant...", tx.Txid)

		if ed.eventConsumerTimeout < 0 {
			select {
			case reg.Eventch <- NewTxStatusEvent(tx.Txid, tx.TxValidationCode):
			default:
				logger.Warnf("Unable to send to Tx Status event channel.")
			}
		} else if ed.eventConsumerTimeout == 0 {
			reg.Eventch <- NewTxStatusEvent(tx.Txid, tx.TxValidationCode)
		} else {
			select {
			case reg.Eventch <- NewTxStatusEvent(tx.Txid, tx.TxValidationCode):
			case <-time.After(ed.eventConsumerTimeout):
				logger.Warnf("Timed out sending Tx Status event.")
			}
		}
	}
}

func (ed *Dispatcher) publishCCEvents(ccEvent *pb.ChaincodeEvent) {
	for _, reg := range ed.ccRegistrations {
		logger.Debugf("Matching CCEvent[%s,%s] against Reg[%s,%s] ...", ccEvent.ChaincodeId, ccEvent.EventName, reg.ChaincodeID, reg.EventFilter)
		if reg.ChaincodeID == ccEvent.ChaincodeId && reg.EventRegExp.MatchString(ccEvent.EventName) {
			logger.Debugf("... matched CCEvent[%s,%s] against Reg[%s,%s]", ccEvent.ChaincodeId, ccEvent.EventName, reg.ChaincodeID, reg.EventFilter)

			if ed.eventConsumerTimeout < 0 {
				select {
				case reg.Eventch <- NewChaincodeEvent(ccEvent.ChaincodeId, ccEvent.EventName, ccEvent.TxId):
				default:
					logger.Warnf("Unable to send to CC event channel.")
				}
			} else if ed.eventConsumerTimeout == 0 {
				reg.Eventch <- NewChaincodeEvent(ccEvent.ChaincodeId, ccEvent.EventName, ccEvent.TxId)
			} else {
				select {
				case reg.Eventch <- NewChaincodeEvent(ccEvent.ChaincodeId, ccEvent.EventName, ccEvent.TxId):
				case <-time.After(ed.eventConsumerTimeout):
					logger.Warnf("Timed out sending CC event.")
				}
			}
		}
	}
}

// RegisterHandler registers an event handler
func (ed *Dispatcher) RegisterHandler(t interface{}, h Handler) {
	htype := reflect.TypeOf(t)
	if _, ok := ed.handlers[htype]; !ok {
		logger.Debugf("Registering handler for %s on dispatcher %T", htype, ed)
		ed.handlers[htype] = h
	} else {
		logger.Debugf("Cannot register handler %s on dispatcher %T since it's already registered", htype, ed)
	}
}

func getCCKey(ccID, eventFilter string) string {
	return ccID + "/" + eventFilter
}

func toFilteredBlock(block *cb.Block) *pb.FilteredBlock {
	var channelID string
	var filteredTxs []*pb.FilteredTransaction
	txFilter := ledgerutil.TxValidationFlags(block.Metadata.Metadata[cb.BlockMetadataIndex_TRANSACTIONS_FILTER])

	for i, data := range block.Data.Data {
		filteredTx, chID, err := getFilteredTx(data, txFilter.Flag(i))
		if err != nil {
			logger.Warnf("error extracting Envelope from block: %v", err)
			continue
		}
		channelID = chID
		filteredTxs = append(filteredTxs, filteredTx)
	}

	return &pb.FilteredBlock{
		ChannelId:  channelID,
		Number:     block.Header.Number,
		FilteredTx: filteredTxs,
	}
}

func getFilteredTx(data []byte, txValidationCode pb.TxValidationCode) (*pb.FilteredTransaction, string, error) {
	env, err := utils.GetEnvelopeFromBlock(data)
	if err != nil {
		return nil, "", errors.Wrap(err, "error extracting Envelope from block")
	}
	if env == nil {
		return nil, "", errors.New("nil envelope")
	}

	payload, err := utils.GetPayload(env)
	if err != nil {
		return nil, "", errors.Wrap(err, "error extracting Payload from envelope")
	}

	channelHeaderBytes := payload.Header.ChannelHeader
	channelHeader := &cb.ChannelHeader{}
	if err := proto.Unmarshal(channelHeaderBytes, channelHeader); err != nil {
		return nil, "", errors.Wrap(err, "error extracting ChannelHeader from payload")
	}

	filteredTx := &pb.FilteredTransaction{
		Type:             cb.HeaderType(channelHeader.Type),
		Txid:             channelHeader.TxId,
		TxValidationCode: txValidationCode,
	}

	if cb.HeaderType(channelHeader.Type) == cb.HeaderType_ENDORSER_TRANSACTION {
		actions, err := getFilteredTransactionActions(payload.Data)
		if err != nil {
			return nil, "", errors.Wrap(err, "error getting filtered transaction actions")
		}
		filteredTx.Data = actions
	}
	return filteredTx, channelHeader.ChannelId, nil
}

func getFilteredTransactionActions(data []byte) (*pb.FilteredTransaction_TransactionActions, error) {
	actions := &pb.FilteredTransaction_TransactionActions{
		TransactionActions: &pb.FilteredTransactionActions{},
	}
	tx, err := utils.GetTransaction(data)
	if err != nil {
		return nil, errors.Wrap(err, "error unmarshalling transaction payload")
	}
	chaincodeActionPayload, err := utils.GetChaincodeActionPayload(tx.Actions[0].Payload)
	if err != nil {
		return nil, errors.Wrap(err, "error unmarshalling chaincode action payload")
	}
	propRespPayload, err := utils.GetProposalResponsePayload(chaincodeActionPayload.Action.ProposalResponsePayload)
	if err != nil {
		return nil, errors.Wrap(err, "error unmarshalling response payload")
	}
	ccAction, err := utils.GetChaincodeAction(propRespPayload.Extension)
	if err != nil {
		return nil, errors.Wrap(err, "error unmarshalling chaincode action")
	}
	ccEvent, err := utils.GetChaincodeEvents(ccAction.Events)
	if err != nil {
		return nil, errors.Wrap(err, "error getting chaincode events")
	}
	if ccEvent != nil {
		actions.TransactionActions.ChaincodeActions = append(actions.TransactionActions.ChaincodeActions, &pb.FilteredChaincodeAction{CcEvent: ccEvent})
	}
	return actions, nil
}

func (ed *Dispatcher) getState() int32 {
	return atomic.LoadInt32(&ed.state)
}

func (ed *Dispatcher) setState(expectedState, newState int32) bool {
	return atomic.CompareAndSwapInt32(&ed.state, expectedState, newState)
}
