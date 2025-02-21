package e2e

import (
	"context"
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"

	"0fatih/yui-ibc-solidity/pkg/client"
	channeltypes "0fatih/yui-ibc-solidity/pkg/ibc/core/channel"
	clienttypes "0fatih/yui-ibc-solidity/pkg/ibc/core/client"
	ibctesting "0fatih/yui-ibc-solidity/pkg/testing"

	"github.com/avast/retry-go"
	"github.com/stretchr/testify/suite"
)

const (
	delayPeriodExtensionA = 5
	delayPeriodExtensionB = 10
)

type ChainTestSuite struct {
	suite.Suite

	coordinator ibctesting.Coordinator
	chainA      *ibctesting.Chain
	chainB      *ibctesting.Chain
}

func (suite *ChainTestSuite) SetupTest() {
	clientA, err := client.NewETHClient("http://127.0.0.1:8645")
	suite.Require().NoError(err)
	clientB, err := client.NewETHClient("http://127.0.0.1:8745")
	suite.Require().NoError(err)

	suite.chainA = ibctesting.NewChain(suite.T(), clientA, ibctesting.NewLightClient(clientA, clienttypes.BesuIBFT2Client))
	suite.chainB = ibctesting.NewChain(suite.T(), clientB, ibctesting.NewLightClient(clientB, clienttypes.BesuIBFT2Client))
	suite.coordinator = ibctesting.NewCoordinator(suite.T(), suite.chainA, suite.chainB)
}

func (suite ChainTestSuite) TestChannel() {
	ctx := context.Background()

	const (
		relayer          = ibctesting.RelayerKeyIndex // the key-index of relayer on both chains
		deployerA        = ibctesting.RelayerKeyIndex // the key-index of contract deployer on chain A
		deployerB        = ibctesting.RelayerKeyIndex // the key-index of contract deployer on chain B
		aliceA    uint32 = 1                          // the key-index of alice on chain A
		bobB      uint32 = 2                          // the key-index of alice on chain B
	)

	chainA := suite.chainA
	chainB := suite.chainB

	clientA, clientB := suite.coordinator.SetupClients(ctx, chainA, chainB, clienttypes.BesuIBFT2Client)
	connA, connB := suite.coordinator.CreateConnection(ctx, chainA, chainB, clientA, clientB)
	chanA, chanB := suite.coordinator.CreateChannel(ctx, chainA, chainB, connA, connB, ibctesting.TransferPort, ibctesting.TransferPort, channeltypes.UNORDERED)

	var delayStartTimeForRecv time.Time
	var delayStartTimeForAck time.Time

	beforeLatestHeight := chainA.GetIBFT2ClientState(clientA).LatestHeight
	beforeConsensusState := chainA.GetIBFT2ConsensusState(clientA, beforeLatestHeight)

	/// Tests for Transfer module ///

	balanceA0, err := chainA.ERC20.BalanceOf(chainA.CallOpts(ctx, relayer), chainA.CallOpts(ctx, deployerA).From)
	suite.Require().NoError(err)
	suite.Require().NoError(chainA.WaitIfNoError(ctx)(
		chainA.ERC20.Approve(chainA.TxOpts(ctx, deployerA), chainA.ContractConfig.ICS20BankAddress, big.NewInt(100)),
	))

	// deposit a simple token to the bank
	suite.Require().NoError(chainA.WaitIfNoError(ctx)(chainA.ICS20Bank.Deposit(
		chainA.TxOpts(ctx, deployerA),
		chainA.ContractConfig.ERC20TokenAddress,
		big.NewInt(100),
		chainA.CallOpts(ctx, aliceA).From,
	)))

	// ensure that the balance is reduced
	balanceA1, err := chainA.ERC20.BalanceOf(chainA.CallOpts(ctx, relayer), chainA.CallOpts(ctx, deployerA).From)
	suite.Require().NoError(err)
	suite.Require().Equal(balanceA0.Int64()-100, balanceA1.Int64())

	baseDenom := strings.ToLower(chainA.ContractConfig.ERC20TokenAddress.String())

	bankA, err := chainA.ICS20Bank.BalanceOf(chainA.CallOpts(ctx, relayer), chainA.CallOpts(ctx, aliceA).From, baseDenom)
	suite.Require().NoError(err)
	suite.Require().GreaterOrEqual(bankA.Int64(), int64(100))

	// set expectedTimePerBlock = block time on chainA
	suite.Require().NoError(chainA.WaitIfNoError(ctx)(
		chainA.IBCHandler.SetExpectedTimePerBlock(
			chainA.TxOpts(ctx, deployerA),
			ibctesting.BlockTime,
		)))
	expectedTimePerBlockA, err := chainA.IBCHandler.GetExpectedTimePerBlock(chainA.CallOpts(ctx, deployerA))
	suite.Require().NoError(err)
	suite.Require().Equal(expectedTimePerBlockA, ibctesting.BlockTime)

	// set expectedTimePerBlock = 0 on chainB
	suite.Require().NoError(chainB.WaitIfNoError(ctx)(
		chainB.IBCHandler.SetExpectedTimePerBlock(
			chainB.TxOpts(ctx, deployerB),
			0,
		)))
	expectedTimePerBlockB, err := chainB.IBCHandler.GetExpectedTimePerBlock(chainB.CallOpts(ctx, deployerB))
	suite.Require().NoError(err)
	suite.Require().Zero(expectedTimePerBlockB)

	// try to transfer the token to chainB
	suite.Require().NoError(chainA.WaitIfNoError(ctx)(
		chainA.ICS20Transfer.SendTransfer(
			chainA.TxOpts(ctx, aliceA),
			baseDenom,
			100,
			chainB.CallOpts(ctx, bobB).From,
			chanA.PortID, chanA.ID,
			uint64(chainB.LastHeader().Number.Int64())+1000,
		),
	))
	chainA.UpdateHeader()
	delayStartTimeForRecv = time.Now()
	suite.Require().NoError(suite.coordinator.UpdateClient(ctx, chainB, chainA, clientB))

	// ensure that escrow has correct balance
	escrowBalance, err := chainA.ICS20Bank.BalanceOf(chainA.CallOpts(ctx, aliceA), chainA.ContractConfig.ICS20TransferBankAddress, baseDenom)
	suite.Require().NoError(err)
	suite.Require().GreaterOrEqual(escrowBalance.Int64(), int64(100))

	// relay the packet
	transferPacket, err := chainA.GetLastSentPacket(ctx, chanA.PortID, chanA.ID)
	suite.Require().NoError(err)
	suite.Require().NoError(retry.Do(
		func() error {
			delayStartTimeForAck = time.Now()
			return suite.coordinator.HandlePacketRecv(ctx, chainB, chainA, chanB, chanA, *transferPacket)
		},
		retry.Delay(time.Second),
		retry.Attempts(60),
	))
	delayForRecv := time.Since(delayStartTimeForRecv)
	suite.T().Log("delay for recv@chainB", delayForRecv)
	suite.Require().Greater(delayForRecv, time.Duration(ibctesting.DefaultDelayPeriod))
	suite.Require().NoError(retry.Do(
		func() error {
			return suite.coordinator.HandlePacketAcknowledgement(ctx, chainA, chainB, chanA, chanB, *transferPacket, []byte{1})
		},
		retry.Delay(time.Second),
		retry.Attempts(60),
	))
	delayForAck := time.Since(delayStartTimeForAck)
	suite.T().Log("delay for ack@chainA", delayForAck)
	suite.Require().Greater(delayForAck, time.Duration(ibctesting.DefaultDelayPeriod))

	// ensure that chainB has correct balance
	expectedDenom := fmt.Sprintf("%v/%v/%v", chanB.PortID, chanB.ID, baseDenom)
	balance, err := chainB.ICS20Bank.BalanceOf(chainB.CallOpts(ctx, relayer), chainB.CallOpts(ctx, bobB).From, expectedDenom)
	suite.Require().NoError(err)
	suite.Require().Equal(int64(100), balance.Int64())

	// make delay period 10 times longer on chainA
	suite.Require().NoError(chainA.WaitIfNoError(ctx)(
		chainA.IBCHandler.SetExpectedTimePerBlock(
			chainA.TxOpts(ctx, deployerA),
			ibctesting.BlockTime/delayPeriodExtensionA,
		)))
	expectedTimePerBlockA, err = chainA.IBCHandler.GetExpectedTimePerBlock(chainA.CallOpts(ctx, deployerA))
	suite.Require().NoError(err)
	suite.Require().Equal(expectedTimePerBlockA, ibctesting.BlockTime/delayPeriodExtensionA)

	// make delay period 20 times longer on chainB
	suite.Require().NoError(chainB.WaitIfNoError(ctx)(
		chainB.IBCHandler.SetExpectedTimePerBlock(
			chainB.TxOpts(ctx, deployerB),
			ibctesting.BlockTime/delayPeriodExtensionB,
		)))
	expectedTimePerBlockB, err = chainB.IBCHandler.GetExpectedTimePerBlock(chainB.CallOpts(ctx, deployerB))
	suite.Require().NoError(err)
	suite.Require().Equal(expectedTimePerBlockB, ibctesting.BlockTime/delayPeriodExtensionB)

	// try to transfer the token to chainA
	suite.Require().NoError(chainB.WaitIfNoError(ctx)(
		chainB.ICS20Transfer.SendTransfer(
			chainB.TxOpts(ctx, bobB),
			expectedDenom,
			100,
			chainA.CallOpts(ctx, aliceA).From,
			chanB.PortID,
			chanB.ID,
			uint64(chainA.LastHeader().Number.Int64())+1000,
		),
	))
	chainB.UpdateHeader()
	delayStartTimeForRecv = time.Now()
	suite.Require().NoError(suite.coordinator.UpdateClient(ctx, chainA, chainB, clientA))

	// relay the packet
	transferPacket, err = chainB.GetLastSentPacket(ctx, chanB.PortID, chanB.ID)
	suite.Require().NoError(err)
	suite.Require().NoError(retry.Do(
		func() error {
			delayStartTimeForAck = time.Now()
			return suite.coordinator.HandlePacketRecv(ctx, chainA, chainB, chanA, chanB, *transferPacket)
		},
		retry.Delay(time.Second),
		retry.Attempts(60),
	))
	delayForRecv = time.Since(delayStartTimeForRecv)
	suite.T().Log("delay for recv@chainA", delayForRecv)
	suite.Require().Greater(delayForRecv, time.Duration(delayPeriodExtensionA*ibctesting.DefaultDelayPeriod))
	suite.Require().NoError(retry.Do(
		func() error {
			return suite.coordinator.HandlePacketAcknowledgement(ctx, chainB, chainA, chanB, chanA, *transferPacket, []byte{1})
		},
		retry.Delay(time.Second),
		retry.Attempts(60),
	))
	delayForAck = time.Since(delayStartTimeForAck)
	suite.T().Log("delay for ack@chainB", delayForAck)
	suite.Require().Greater(delayForAck, time.Duration(delayPeriodExtensionB*ibctesting.DefaultDelayPeriod))

	// withdraw tokens from the bank
	suite.Require().NoError(chainA.WaitIfNoError(ctx)(
		chainA.ICS20Bank.Withdraw(
			chainA.TxOpts(ctx, aliceA),
			chainA.ContractConfig.ERC20TokenAddress,
			big.NewInt(100),
			chainA.CallOpts(ctx, deployerA).From,
		)))

	// ensure that token balance equals original value
	balanceA2, err := chainA.ERC20.BalanceOf(chainA.CallOpts(ctx, relayer), chainA.CallOpts(ctx, deployerA).From)
	suite.Require().NoError(err)
	suite.Require().Equal(balanceA0.Int64(), balanceA2.Int64())

	// close channel
	suite.coordinator.CloseChannel(ctx, chainA, chainB, chanA, chanB)
	// confirm that the channel is CLOSED on chain A
	chanData, ok, err := chainA.IBCHandler.GetChannel(chainA.CallOpts(ctx, relayer), chanA.PortID, chanA.ID)
	suite.Require().NoError(err)
	suite.Require().True(ok)
	suite.Require().Equal(channeltypes.Channel_State(chanData.State), channeltypes.CLOSED)
	// confirm that the channel is CLOSED on chain B
	chanData, ok, err = chainB.IBCHandler.GetChannel(chainB.CallOpts(ctx, relayer), chanB.PortID, chanB.ID)
	suite.Require().NoError(err)
	suite.Require().True(ok)
	suite.Require().Equal(channeltypes.Channel_State(chanData.State), channeltypes.CLOSED)

	afterLatestHeight := chainA.GetIBFT2ClientState(clientA).LatestHeight
	suite.Require().Equal(afterLatestHeight.RevisionNumber, beforeLatestHeight.RevisionNumber)
	suite.Require().True(afterLatestHeight.RevisionHeight > beforeLatestHeight.RevisionHeight)

	beforeConsensusState2 := chainA.GetIBFT2ConsensusState(clientA, beforeLatestHeight)
	suite.Require().Equal(beforeConsensusState, beforeConsensusState2)
}

func TestChainTestSuite(t *testing.T) {
	suite.Run(t, new(ChainTestSuite))
}
