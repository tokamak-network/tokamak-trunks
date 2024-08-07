package trunks

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	vegeta "github.com/tsenart/vegeta/v12/lib"

	"github.com/tokamak-network/tokamak-trunks/reporter"
)

type Attacker interface {
	Attack() <-chan *vegeta.Result
}

type CallAttacker struct {
	Pace     vegeta.Pacer
	Duration time.Duration
	Targeter vegeta.Targeter
}
type TransactionAttacker struct {
	Client   *ethclient.Client
	Pace     vegeta.Pacer
	Duration time.Duration
	Targeter vegeta.Targeter
}

func MakeAttacker(action *Action, t *Trunks) (Attacker, error) {
	duration, err := time.ParseDuration(action.Duration)
	if err != nil {
		return nil, err
	}
	if action.Method == "call" {
		tOption := &TargetOption{
			RPC: t.L2RPC,
		}
		return &CallAttacker{
			Pace:     action.GetPace(),
			Duration: duration,
			Targeter: CallTargeter(tOption),
		}, nil

	}
	if action.Method == "transaction" {
		rpc := t.L2RPC
		chainId := t.L2ChainId
		client, _ := ethclient.Dial(rpc)
		tOption := &TargetOption{
			RPC: rpc,
			TransactionOption: &TransactionOption{
				Accounts: t.Accounts,
				ChainId:  chainId,
				To:       action.To,
				Client:   client,
			},
		}
		return &TransactionAttacker{
			Client:   client,
			Pace:     action.GetPace(),
			Duration: duration,
			Targeter: TransactionTargeter(tOption),
		}, nil
	}

	return nil, fmt.Errorf("wrong action method")
}

func (ca *CallAttacker) Attack() <-chan *vegeta.Result {
	fmt.Println("call attack start")
	attacker := vegeta.NewAttacker()
	results := make(chan *vegeta.Result)
	attackCount := 0
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for res := range attacker.Attack(ca.Targeter, ca.Pace, ca.Duration, "call") {
			attackCount++
			fmt.Printf("\rAttack count: %d", attackCount)
			results <- res
		}
	}()

	go func() {
		wg.Wait()
		defer close(results)
	}()

	return results
}

func (ta *TransactionAttacker) Attack() <-chan *vegeta.Result {
	fmt.Println("transaction attack start")
	attacker := vegeta.NewAttacker()
	results := make(chan *vegeta.Result)
	reporter := reporter.GetTrunksReport()
	attackCount := 0
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for res := range attacker.Attack(ta.Targeter, ta.Pace, ta.Duration, "transaction attack") {
			attackCount++
			fmt.Printf("\rAttack count: %d", attackCount)

			txHash, jsonErr := txHashFromResult(res)
			if jsonErr != nil {
				res.Error = fmt.Sprintf("err: %s", jsonErr.Message)
				res.Code = uint16(jsonErr.Code)
				results <- res
				continue
			}

			wg.Add(1)
			go func(txHash common.Hash, result *vegeta.Result) {
				defer wg.Done()
				ctx, cancel := context.WithTimeout(context.Background(), time.Second*120)
				defer cancel()
				receipt, err := waitTxConfirm(ctx, ta.Client, txHash)
				receiptTime := result.Timestamp.Add(time.Since(result.Timestamp))
				latency := receiptTime.Sub(result.Timestamp)
				result.Latency += latency

				if err != nil {
					result.Error = err.Error()
					result.Code = 0
				}
				if receipt != nil {
					switch receipt.Status {
					case 1:
						reporter.RecordReceipt(receipt)
						reporter.RecordConfirmRequest()
					case 0:
						result.Error = "transaction confirmed faiure"
						result.Code = 0
					}
				}

				results <- result
			}(txHash, res)
		}
		fmt.Println()
	}()

	go func() {
		wg.Wait()
		defer close(results)
	}()

	return results
}

func waitTxConfirm(
	ctx context.Context,
	client *ethclient.Client,
	txHash common.Hash,
) (*types.Receipt, error) {
	queryTicker := time.NewTicker(12 * time.Second)
	defer queryTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-queryTicker.C:
			if receiept, _ := client.TransactionReceipt(ctx, txHash); receiept != nil {
				return receiept, nil
			}
		}
	}
}

type jsonrpcMessage struct {
	Version string          `json:"jsonrpc,omitempty"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Error   *jsonError      `json:"error,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
}

type jsonError struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
}

func txHashFromResult(r *vegeta.Result) (common.Hash, *jsonError) {
	message := jsonrpcMessage{}
	json.Unmarshal(r.Body, &message)

	if message.Error != nil {
		return common.Hash{}, message.Error
	}

	stringTxHash := strings.Trim(string(message.Result), `"`)
	txHash := common.HexToHash(stringTxHash)

	return txHash, nil
}
