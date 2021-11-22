package signumapi

import (
	"errors"
	"fmt"
	"github.com/xDWart/signum-explorer-bot/api/abstractapi"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"
)

const DEFAULT_DEADLINE = 1440

type RequestType string

const (
	RT_SEND_MONEY               RequestType = "sendMoney"          // recipient + amountNQT
	RT_SEND_MONEY_MULTI         RequestType = "sendMoneyMulti"     // recipients = <numid1>:<amount1>;<numid2>:<amount2>;<numidN>:<amountN>
	RT_SEND_MONEY_MULTI_SAME    RequestType = "sendMoneyMultiSame" // recipients = <numid1>;<numid2>;<numidN> + amountNQT
	RT_SEND_MESSAGE             RequestType = "sendMessage"
	RT_READ_MESSAGE             RequestType = "readMessage"
	RT_SUGGEST_FEE              RequestType = "suggestFee"
	RT_GET_ACCOUNT              RequestType = "getAccount"
	RT_GET_TRANSACTION          RequestType = "getTransaction"
	RT_GET_BLOCK                RequestType = "getBlock"
	RT_GET_ACCOUNT_ID           RequestType = "getAccountId"
	RT_GET_ACCOUNT_TRANSACTIONS RequestType = "getAccountTransactions"
	RT_GET_MINING_INFO          RequestType = "getMiningInfo"
	RT_GET_BLOCKCHAIN_STATUS    RequestType = "getBlockchainStatus"
	RT_GET_REWARD_RECIPIENT     RequestType = "getRewardRecipient"
	RT_SET_REWARD_RECIPIENT     RequestType = "setRewardRecipient"
	RT_ADD_COMMITMENT           RequestType = "addCommitment"
	RT_REMOVE_COMMITMENT        RequestType = "removeCommitment"
	RT_SET_ACCOUNT_INFO         RequestType = "setAccountInfo"
)

type SignumApiClient struct {
	apiClientsPool             apiClientsPool
	localAccountCache          AccountCache
	localTransactionsCache     TransactionsCache
	localBlocksCache           BlocksCache
	localSuggestFeeCache       SuggestFeeCache
	localBigWalletNamesCache   BigWalletNamesCache
	localBlockchainStatusCache BlockchainStatusCache
	config                     *Config
	shutdownChannel            chan interface{}
}

type apiClientsPool struct {
	sync.RWMutex
	clients []*apiClient
}

type apiClient struct {
	*abstractapi.AbstractApiClient
	blockchainStatus BlockchainStatus
	latency          time.Duration
}

type Config struct {
	ApiHosts                  []string
	CacheTtl                  time.Duration
	LastIndex                 uint64
	RebuildApiClientsPeriod   time.Duration
	PreloadNamesForBigWallets bool
}

func NewSignumApiClient(logger abstractapi.LoggerI, wg *sync.WaitGroup, shutdownChannel chan interface{}, config *Config) *SignumApiClient {
	rand.Seed(time.Now().UnixNano())

	apiClients := make([]*apiClient, 0, len(config.ApiHosts))
	for _, host := range config.ApiHosts {
		apiClients = append(apiClients, &apiClient{
			AbstractApiClient: abstractapi.NewAbstractApiClient(host, nil),
		})
	}

	signumApiClient := &SignumApiClient{
		apiClientsPool:           apiClientsPool{clients: apiClients},
		localAccountCache:        AccountCache{sync.RWMutex{}, map[string]*Account{}},
		localTransactionsCache:   TransactionsCache{sync.RWMutex{}, map[string]map[TransactionType]map[TransactionSubType]*AccountTransactions{}},
		localBlocksCache:         BlocksCache{sync.RWMutex{}, map[string]*AccountBlocks{}},
		localBigWalletNamesCache: BigWalletNamesCache{sync.RWMutex{}, map[string]string{}},
		shutdownChannel:          shutdownChannel,
		config:                   config,
	}

	wg.Add(1)
	go signumApiClient.startApiClientsRebuilder(logger, wg)

	return signumApiClient
}

func (c *SignumApiClient) Stop() {
	close(c.shutdownChannel)
}

func (c *SignumApiClient) startApiClientsRebuilder(logger abstractapi.LoggerI, wg *sync.WaitGroup) {
	defer wg.Done()

	logger.Infof("Start Signum Api Clients Rebuilder")
	ticker := time.NewTicker(c.config.RebuildApiClientsPeriod)

	c.rebuildApiClients(logger)
	if c.config.PreloadNamesForBigWallets {
		c.preloadNamesForBigWallets(logger)
	}

	for {
		select {
		case <-c.shutdownChannel:
			logger.Infof("Signum Api Clients Rebuilder received shutdown signal")
			ticker.Stop()
			return

		case <-ticker.C:
			c.rebuildApiClients(logger)
		}
	}
}

func (c *SignumApiClient) rebuildApiClients(logger abstractapi.LoggerI) {
	newApiClients := upbuildApiClients(logger, c.config.ApiHosts)
	if len(newApiClients) > 0 {
		c.apiClientsPool.Lock()
		c.apiClientsPool.clients = newApiClients
		c.apiClientsPool.Unlock()
	} else {
		logger.Errorf("Could not rebuild api clients")
	}
}

func upbuildApiClients(logger abstractapi.LoggerI, apiHosts []string) []*apiClient {
	logger.Infof("Start rebuilding Signum Api Clients")
	startTime := time.Now()

	clients := make([]*apiClient, 0, len(apiHosts))
	for _, host := range apiHosts {
		client := &apiClient{
			AbstractApiClient: abstractapi.NewAbstractApiClient(host, nil),
		}
		startTime := time.Now()
		err := client.DoJsonReq(logger, "GET", "/burst",
			map[string]string{"requestType": string(RT_GET_BLOCKCHAIN_STATUS)}, nil, &client.blockchainStatus)
		if err != nil {
			logger.Warnf("Failed DoJsonReq: %v", err)
			continue
		}
		client.latency = time.Since(startTime)
		clients = append(clients, client)
		logger.Debugf("Signum Api Clients Rebuilder requested %v (%v) for %v",
			client.ApiHost, client.blockchainStatus.NumberOfBlocks, client.latency)
	}
	sort.Slice(clients, func(i, j int) bool {
		// allow out of sync in 1 block
		if clients[i].blockchainStatus.NumberOfBlocks-1 > clients[j].blockchainStatus.NumberOfBlocks {
			return true
		}
		if clients[i].blockchainStatus.NumberOfBlocks < clients[j].blockchainStatus.NumberOfBlocks-1 {
			return false
		}
		return clients[i].latency < clients[j].latency
	})

	logger.Infof("Signum Api Clients has been rebuilt in %v", time.Since(startTime))
	return clients
}

func (c *SignumApiClient) doJsonReq(logger abstractapi.LoggerI, httpMethod string, method string, urlParams map[string]string, additionalHeaders map[string]string, output interface{}) error {
	c.apiClientsPool.RLock()
	apiClients := c.apiClientsPool.clients
	c.apiClientsPool.RUnlock()

	rand.Shuffle(len(apiClients)/2, func(i, j int) { apiClients[i], apiClients[j] = apiClients[j], apiClients[i] })

	var err error
	for _, apiClient := range apiClients {
		err = apiClient.DoJsonReq(logger, httpMethod, method, urlParams, additionalHeaders, output)
		if err != nil {
			securedErrorMsg := deleteSubstr(err.Error(), "secretPhrase=", "\"")
			err = errors.New(securedErrorMsg)
			logger.Warnf("AbstractApiClient.DoJsonReq error: %v", err)
			if httpMethod == "POST" &&
				!strings.Contains(err.Error(), "connection refused") &&
				!strings.Contains(err.Error(), "host unreachable") &&
				!strings.Contains(err.Error(), "TLS handshake timeout") &&
				!strings.Contains(err.Error(), "remote error") &&
				!strings.Contains(err.Error(), "StatusCode") &&
				!strings.Contains(err.Error(), "certificate has expired") {
				return err
			}
			continue
		}
		return nil
	}
	return fmt.Errorf("couldn't get %v method: %v", method, err)
}

func deleteSubstr(input, from, to string) string {
	var start = strings.Index(input, from)
	if start <= 0 {
		return input
	}

	var length = strings.Index(input[start:], to)
	if length < 0 {
		return input[:start]
	}

	return input[:start] + input[start+length:]
}
