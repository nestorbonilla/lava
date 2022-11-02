package chainproxy

import (
	"context"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcec"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/lavanet/lava/relayer/chainproxy/rpcclient"
	"github.com/lavanet/lava/relayer/lavasession"
	"github.com/lavanet/lava/relayer/sentry"
	"github.com/lavanet/lava/relayer/sigs"
	pairingtypes "github.com/lavanet/lava/x/pairing/types"
	spectypes "github.com/lavanet/lava/x/spec/types"
)

const (
	DefaultTimeout = 5 * time.Second
)

type NodeMessage interface {
	GetServiceApi() *spectypes.ServiceApi
	Send(ctx context.Context, ch chan interface{}) (relayReply *pairingtypes.RelayReply, subscriptionID string, relayReplyServer *rpcclient.ClientSubscription, err error)
	RequestedBlock() int64
	GetMsg() interface{}
}

type ChainProxy interface {
	Start(context.Context) error
	GetSentry() *sentry.Sentry
	ParseMsg(string, []byte, string) (NodeMessage, error)
	PortalStart(context.Context, *btcec.PrivateKey, string)
	FetchLatestBlockNum(ctx context.Context) (int64, error)
	FetchBlockHashByNum(ctx context.Context, blockNum int64) (string, error)
	GetConsumerSessionManager() *lavasession.ConsumerSessionManager
}

func GetChainProxy(nodeUrl string, nConns uint, sentry *sentry.Sentry) (ChainProxy, error) {
	consumerSessionManagerInstance := &lavasession.ConsumerSessionManager{}
	switch sentry.ApiInterface {
	case "jsonrpc":
		return NewJrpcChainProxy(nodeUrl, nConns, sentry, consumerSessionManagerInstance), nil
	case "tendermintrpc":
		return NewtendermintRpcChainProxy(nodeUrl, nConns, sentry, consumerSessionManagerInstance), nil
	case "rest":
		return NewRestChainProxy(nodeUrl, sentry, consumerSessionManagerInstance), nil
	}
	return nil, fmt.Errorf("chain proxy for apiInterface (%s) not found", sentry.ApiInterface)
}

func VerifyRelayReply(reply *pairingtypes.RelayReply, relayRequest *pairingtypes.RelayRequest, addr string, comparesHashes bool) error {

	serverKey, err := sigs.RecoverPubKeyFromRelayReply(reply, relayRequest)
	if err != nil {
		return err
	}
	serverAddr, err := sdk.AccAddressFromHex(serverKey.Address().String())
	if err != nil {
		return err
	}
	if serverAddr.String() != addr {
		return fmt.Errorf("server address mismatch in reply (%s) (%s)", serverAddr.String(), addr)
	}

	if comparesHashes {
		strAdd, err := sdk.AccAddressFromBech32(addr)
		if err != nil {
			return err
		}
		serverKey, err = sigs.RecoverPubKeyFromResponseFinalizationData(reply, relayRequest, strAdd)
		if err != nil {
			return err
		}

		serverAddr, err = sdk.AccAddressFromHex(serverKey.Address().String())
		if err != nil {
			return err
		}

		if serverAddr.String() != strAdd.String() {
			return fmt.Errorf("server address mismatch in reply sigblocks (%s) (%s)", serverAddr.String(), strAdd.String())
		}
	}
	return nil
}

func UpdateAllProvidersCallback(cp ChainProxy, ctx context.Context, epoch uint64, pairingList []*lavasession.ConsumerSessionsWithProvider) error {
	return cp.GetConsumerSessionManager().UpdateAllProviders(ctx, epoch, pairingList)
}

// Client requests and queries
func SendRelay(
	ctx context.Context,
	cp ChainProxy,
	privKey *btcec.PrivateKey,
	url string,
	req string,
	connectionType string,
) (*pairingtypes.RelayReply, *pairingtypes.Relayer_RelaySubscribeClient, error) {

	// Unmarshal request
	nodeMsg, err := cp.ParseMsg(url, []byte(req), connectionType)
	if err != nil {
		return nil, nil, err
	}
	isSubscription := nodeMsg.GetServiceApi().Category.Subscription
	blockHeight := int64(-1) //to sync reliability blockHeight in case it changes
	requestedBlock := int64(0)

	// Get Session. we get session here so we can use the epoch in the callbacks
	consumerSession, epoch, providerPublicAddress, reportedProviders, err := cp.GetConsumerSessionManager().GetSession(ctx, nodeMsg.GetServiceApi().ComputeUnits, nil)
	if err != nil {
		return nil, nil, err
	}
	// consumerSession is locked here.

	callback_send_relay := func(consumerSession *lavasession.SingleConsumerSession, unresponsiveProviders []byte) (*pairingtypes.RelayReply, *pairingtypes.Relayer_RelaySubscribeClient, *pairingtypes.RelayRequest, error) {
		//client session is locked here
		blockHeight = int64(epoch) // epochs heights only

		relayRequest := &pairingtypes.RelayRequest{
			Provider:              providerPublicAddress,
			ConnectionType:        connectionType,
			ApiUrl:                url,
			Data:                  []byte(req),
			SessionId:             uint64(consumerSession.SessionId),
			ChainID:               cp.GetSentry().ChainID,
			CuSum:                 consumerSession.CuSum,
			BlockHeight:           blockHeight,
			RelayNum:              consumerSession.RelayNum,
			RequestBlock:          nodeMsg.RequestedBlock(),
			QoSReport:             consumerSession.QoSInfo.LastQoSReport,
			DataReliability:       nil,
			UnresponsiveProviders: unresponsiveProviders,
		}
		// TODO_RAN: fix here when finished with sentry
		sig, err := sigs.SignRelay(privKey, *relayRequest)
		if err != nil {
			return nil, nil, nil, err
		}
		relayRequest.Sig = sig
		c := *consumerSession.Endpoint.Client

		relaySentTime := time.Now()
		consumerSession.QoSInfo.TotalRelays++
		connectCtx, cancel := context.WithTimeout(ctx, DefaultTimeout)
		defer cancel()

		var replyServer pairingtypes.Relayer_RelaySubscribeClient
		var reply *pairingtypes.RelayReply

		if isSubscription {
			replyServer, err = c.RelaySubscribe(ctx, relayRequest)
		} else {
			reply, err = c.Relay(connectCtx, relayRequest)
		}

		if err != nil {
			if err.Error() == context.DeadlineExceeded.Error() {
				consumerSession.QoSInfo.ConsecutiveTimeOut++
			}
			return nil, nil, nil, err
		}

		if !isSubscription {
			currentLatency := time.Since(relaySentTime)
			consumerSession.QoSInfo.ConsecutiveTimeOut = 0
			consumerSession.QoSInfo.AnsweredRelays++

			//update relay request requestedBlock to the provided one in case it was arbitrary
			sentry.UpdateRequestedBlock(relayRequest, reply)
			requestedBlock = relayRequest.RequestBlock

			err = VerifyRelayReply(reply, relayRequest, consumerSession.Client.Acc, cp.GetSentry().GetSpecComparesHashes())
			if err != nil {
				return nil, nil, nil, err
			}

			expectedBH, numOfProviders := cp.GetSentry().ExpectedBlockHeight()
			consumerSession.CalculateQoS(nodeMsg.GetServiceApi().ComputeUnits, currentLatency, expectedBH-reply.LatestBlock, numOfProviders, int64(cp.GetSentry().GetProvidersCount()))
		}

		return reply, &replyServer, relayRequest, nil
	}

	callback_send_reliability := func(consumerSession *lavasession.SingleConsumerSession, dataReliability *pairingtypes.VRFData, unresponsiveProviders []byte) (*pairingtypes.RelayReply, *pairingtypes.RelayRequest, error) {
		//client session is locked here
		sentry := cp.GetSentry()
		if blockHeight < 0 {
			return nil, nil, fmt.Errorf("expected callback_send_relay to be called first and set blockHeight")
		}

		relayRequest := &pairingtypes.RelayRequest{
			Provider:              consumerSession.Client.Acc,
			ApiUrl:                url,
			Data:                  []byte(req),
			SessionId:             uint64(0), //sessionID for reliability is 0
			ChainID:               sentry.ChainID,
			CuSum:                 consumerSession.CuSum,
			BlockHeight:           blockHeight,
			RelayNum:              consumerSession.RelayNum,
			RequestBlock:          requestedBlock,
			QoSReport:             nil,
			DataReliability:       dataReliability,
			ConnectionType:        connectionType,
			UnresponsiveProviders: unresponsiveProviders,
		}

		sig, err := sigs.SignRelay(privKey, *relayRequest)
		if err != nil {
			return nil, nil, err
		}
		relayRequest.Sig = sig

		sig, err = sigs.SignVRFData(privKey, relayRequest.DataReliability)
		if err != nil {
			return nil, nil, err
		}
		relayRequest.DataReliability.Sig = sig
		c := *consumerSession.Endpoint.Client
		reply, err := c.Relay(ctx, relayRequest)
		if err != nil {
			return nil, nil, err
		}

		err = VerifyRelayReply(reply, relayRequest, consumerSession.Client.Acc, cp.GetSentry().GetSpecComparesHashes())
		if err != nil {
			return nil, nil, err
		}

		return reply, relayRequest, nil
	}

	reply, replyServer, err := cp.GetSentry().SendRelay(ctx, consumerSession, reportedProviders, epoch, providerPublicAddress, callback_send_relay, callback_send_reliability, nodeMsg.GetServiceApi().Category)
	if err != nil {
		// on session failure here
		if lavasession.SendRelayError.Is(err) {
			// send again?
		}
	}

	latestBlock := reply.LatestBlock
	cp.GetConsumerSessionManager().OnSessionDone(consumerSession, epoch, latestBlock) // session done successfully

	return reply, replyServer, err
}
