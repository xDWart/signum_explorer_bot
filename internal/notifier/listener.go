package notifier

import (
	"fmt"
	"github.com/xDWart/signum-explorer-bot/api/signumapi"
	"github.com/xDWart/signum-explorer-bot/internal/common"
	"github.com/xDWart/signum-explorer-bot/internal/config"
	"github.com/xDWart/signum-explorer-bot/internal/database/models"
	"log"
	"strings"
	"sync"
	"time"
)

const NOTIFIER_PERIOD = 4 * time.Minute
const NOTIFIER_CHECK_BLOCKS_PER = 3 // 4 min * 3 = per 12 min

func (n *Notifier) startListener(wg *sync.WaitGroup, shutdownChannel chan interface{}) {
	defer wg.Done()

	log.Printf("Start Notifier")
	ticker := time.NewTicker(NOTIFIER_PERIOD)

	var counter uint

	n.checkAccounts(true)
	for {
		select {
		case <-shutdownChannel:
			log.Printf("Notify Listener received shutdown signal")
			ticker.Stop()
			return

		case <-ticker.C:
			counter++
			checkBlocks := counter%NOTIFIER_CHECK_BLOCKS_PER == 0
			n.checkAccounts(checkBlocks)
		}
	}
}

func (n *Notifier) checkAccounts(checkBlocks bool) {
	var monitoredAccounts []MonitoredAccount

	err := n.db.Model(&models.DbUser{}).Select("*").
		Joins("join db_accounts on db_accounts.db_user_id = db_users.id").
		Where("db_accounts.notify_income_transactions = true OR db_accounts.notify_outgo_transactions = true " +
			"OR db_accounts.notify_new_blocks = true OR db_accounts.notify_other_t_xs = true").
		Scan(&monitoredAccounts).Error
	if err != nil {
		log.Printf("Can't get monitored accounts: %v", err)
		return
	}

	for _, account := range monitoredAccounts {
		log.Printf("Notifier will request data for account %v (intx %v, outtx %v, block %v)", account.AccountRS,
			account.NotifyIncomeTransactions, account.NotifyOutgoTransactions, account.NotifyNewBlocks)

		if account.NotifyIncomeTransactions || account.NotifyOutgoTransactions {
			n.checkPaymentTransactions(&account)
		}

		if account.NotifyOtherTXs {
			n.checkMiningTransactions(&account)
			n.checkMessageTransactions(&account)
		}

		if checkBlocks && account.NotifyNewBlocks {
			n.checkBlocks(&account)
		}
	}
}

func (n *Notifier) checkMiningTransactions(account *MonitoredAccount) {
	userTransactions, err := n.signumClient.GetCachedAccountMiningTransactions(account.Account)
	if err != nil {
		log.Printf("Can't get last account %v mining transactions: %v", account.Account, err)
		return
	}

	if userTransactions == nil || len(userTransactions.Transactions) == 0 {
		return
	}

	if userTransactions.Transactions[0].TransactionID == account.LastMiningTX {
		return
	}

	for _, transaction := range userTransactions.Transactions {
		if transaction.TransactionID == account.LastMiningTX {
			break
		}
		msg := fmt.Sprintf("📝 <b>%v</b> ", account.AccountRS)

		var totalCommitment string
		newAccount, err := n.signumClient.GetAccount(account.Account)
		if err == nil {
			totalCommitment = fmt.Sprintf("\n<b>Total commitment: %v SIGNA</b>", common.FormatNumber(newAccount.CommittedBalance, 2))
		}

		switch transaction.Subtype {
		case signumapi.TST_REWARD_RECIPIENT_ASSIGNMENT:
			var recipientName string
			recipientAccount, err := n.signumClient.GetCachedAccount(transaction.Recipient)
			if err == nil && recipientAccount.Name != "" {
				recipientName = fmt.Sprintf("\n<i>Name:</i> %v", recipientAccount.Name)
			}

			msg += fmt.Sprintf("new recipient assigned:"+
				"\n<i>Recipient:</i> %v"+recipientName+
				"\n<i>Fee:</i> %v SIGNA",
				transaction.RecipientRS, transaction.FeeNQT/1e8)
		case signumapi.TST_ADD_COMMITMENT:
			msg += fmt.Sprintf("new commitment added:"+
				"\n<i>Amount:</i> +%v SIGNA"+
				"\n<i>Fee:</i> %v SIGNA",
				common.FormatNumber(transaction.Attachment.AmountNQT/1e8, 2), transaction.FeeNQT/1e8)
		case signumapi.TST_REMOVE_COMMITMENT:
			msg += fmt.Sprintf("commitment revoked:"+
				"\n<i>Amount:</i> -%v SIGNA"+
				"\n<i>Fee:</i> %v SIGNA",
				common.FormatNumber(transaction.Attachment.AmountNQT/1e8, 2), transaction.FeeNQT/1e8)
		default:
			log.Printf("%v: unknown SubType (%v) for transaction %v", account.Account, transaction.Subtype, transaction.TransactionID)
			continue
		}

		n.notifierCh <- NotifierMessage{
			UserName: account.UserName,
			ChatID:   account.ChatID,
			Message:  msg + totalCommitment,
		}
	}

	account.DbAccount.LastMiningTX = userTransactions.Transactions[0].TransactionID
	n.db.Save(&account.DbAccount)
}

func (n *Notifier) checkPaymentTransactions(account *MonitoredAccount) {
	userTransactions, err := n.signumClient.GetCachedAccountPaymentTransactions(account.Account)
	if err != nil {
		log.Printf("Can't get last account %v payment transactions: %v", account.Account, err)
		return
	}

	if userTransactions == nil || len(userTransactions.Transactions) == 0 {
		return
	}

	if userTransactions.Transactions[0].TransactionID == account.LastTransactionID {
		return
	}

	var totalBalance string
	newAccount, err := n.signumClient.GetAccount(account.Account)
	if err == nil {
		totalBalance = fmt.Sprintf("\n<b>Total balance: %v SIGNA</b>", common.FormatNumber(newAccount.TotalBalance, 2))
	}

	for _, transaction := range userTransactions.Transactions {
		if transaction.TransactionID == account.LastTransactionID {
			break
		}

		// ignore RAFFLE
		if transaction.SenderRS == "S-JM3M-MHWM-UVQ6-DSN3Q" {
			continue
		}

		msg := fmt.Sprintf("💸 <b>%v</b> ", account.AccountRS)

		var incomeTransaction = transaction.Sender != account.Account
		var name string
		if incomeTransaction {
			if !account.NotifyIncomeTransactions {
				continue
			}

			senderAccount, err := n.signumClient.GetCachedAccount(transaction.SenderRS)
			if err == nil && senderAccount.Name != "" {
				name = fmt.Sprintf("\n<i>Name:</i> %v", senderAccount.Name)
			}
		} else if account.NotifyOutgoTransactions { // outgo
			if transaction.RecipientRS != "" {
				recipientAccount, err := n.signumClient.GetCachedAccount(transaction.RecipientRS)
				if err == nil && recipientAccount.Name != "" {
					name = fmt.Sprintf("\n<i>Name:</i> %v", recipientAccount.Name)
				}
			}
		} else {
			continue
		}

		var message string
		if transaction.Attachment.MessageIsText && transaction.Attachment.Message != "" {
			if len([]rune(transaction.Attachment.Message)) > 32 {
				transaction.Attachment.Message = string([]rune(transaction.Attachment.Message)[:32])
				transaction.Attachment.Message = strings.ReplaceAll(transaction.Attachment.Message, "\n", " ")
				transaction.Attachment.Message += "..."
			}
			message = fmt.Sprintf("\n<i>Message:</i> %v", transaction.Attachment.Message)
		} else if transaction.Attachment.EncryptedMessage != nil {
			message = fmt.Sprintf("\n<i>Message:</i> [encrypted]")
		}

		var amount float64
		var outgoAccount string
		var outgoAccountRS string
		switch transaction.Subtype {
		case signumapi.TST_ORDINARY_PAYMENT:
			amount = transaction.AmountNQT / 1e8
			if incomeTransaction {
				msg += fmt.Sprintf("new income:"+
					"\n<i>Payment:</i> Ordinary"+
					"\n<i>Sender:</i> %v"+
					name+
					"\n<i>Amount:</i> +%v SIGNA"+
					message+
					"\n<i>Fee:</i> %v SIGNA",
					transaction.SenderRS, common.FormatNumber(amount, 2), transaction.FeeNQT/1e8)
			} else {
				msg += fmt.Sprintf("new outgo:"+
					"\n<i>Payment:</i> Ordinary"+
					"\n<i>Recipient:</i> %v"+
					name+
					"\n<i>Amount:</i> -%v SIGNA"+
					message+
					"\n<i>Fee:</i> %v SIGNA",
					transaction.RecipientRS, common.FormatNumber(amount, 2), transaction.FeeNQT/1e8)
				outgoAccount = transaction.Recipient
				outgoAccountRS = transaction.RecipientRS
			}
		case signumapi.TST_MULTI_OUT_PAYMENT:
			if incomeTransaction {
				amount = transaction.Attachment.Recipients.FoundMyAmount(account.Account)
				msg += fmt.Sprintf("new income:"+
					"\n<i>Payment:</i> Multi-out"+
					"\n<i>Sender:</i> %v"+
					name+
					"\n<i>Amount:</i> +%v SIGNA"+
					message+
					"\n<i>Fee:</i> %v SIGNA",
					transaction.SenderRS, common.FormatNumber(amount, 2), transaction.FeeNQT/1e8)
			} else {
				amount = transaction.AmountNQT / 1e8
				msg += fmt.Sprintf("new outgo:"+
					"\n<i>Payment:</i> Multi-out"+
					"\n<i>Recipients:</i> %v"+
					"\n<i>Amount:</i> -%v SIGNA"+
					message+
					"\n<i>Fee:</i> %v SIGNA",
					len(transaction.Attachment.Recipients), common.FormatNumber(amount, 2), transaction.FeeNQT/1e8)
			}
		case signumapi.TST_MULTI_OUT_SAME_PAYMENT:
			if incomeTransaction {
				amount = transaction.AmountNQT / 1e8 / float64(len(transaction.Attachment.Recipients))
				msg += fmt.Sprintf("new income:"+
					"\n<i>Payment:</i> Multi-out same"+
					"\n<i>Sender:</i> %v"+
					name+
					"\n<i>Amount:</i> +%v SIGNA"+
					message+
					"\n<i>Fee:</i> %v SIGNA",
					transaction.SenderRS, common.FormatNumber(amount, 2), transaction.FeeNQT/1e8)
			} else {
				amount = transaction.AmountNQT / 1e8
				msg += fmt.Sprintf("new outgo:"+
					"\n<i>Payment:</i> Multi-out same"+
					"\n<i>Recipients:</i> %v"+
					"\n<i>Amount:</i> -%v SIGNA"+
					message+
					"\n<i>Fee:</i> %v SIGNA",
					len(transaction.Attachment.Recipients), common.FormatNumber(amount, 2), transaction.FeeNQT/1e8)
			}
		default:
			log.Printf("%v: unknown SubType (%v) for transaction %v", account.Account, transaction.Subtype, transaction.TransactionID)
			continue
		}

		if account.AccountRS == config.FAUCET_ACCOUNT {
			if incomeTransaction { // it's donate
				newDonate := models.Donation{
					Account:       transaction.Sender,
					AccountRS:     transaction.SenderRS,
					TransactionID: transaction.TransactionID,
					Amount:        amount,
				}
				n.db.Save(&newDonate)
			} else { // it's faucet
				newFaucet := models.Faucet{
					Account:       outgoAccount,
					AccountRS:     outgoAccountRS,
					TransactionID: transaction.TransactionID,
					Amount:        amount,
					Fee:           transaction.FeeNQT / 1e8,
				}
				n.db.Save(&newFaucet)
			}
		}

		n.notifierCh <- NotifierMessage{
			UserName: account.UserName,
			ChatID:   account.ChatID,
			Message:  msg + totalBalance,
		}
	}

	account.DbAccount.LastTransactionID = userTransactions.Transactions[0].TransactionID
	n.db.Save(&account.DbAccount)
}

func (n *Notifier) checkBlocks(account *MonitoredAccount) {
	userBlocks, err := n.signumClient.GetCachedAccountBlocks(account.Account)
	if err != nil {
		log.Printf("Can't get last account %v blocks: %v", account.Account, err)
		return
	}

	if userBlocks == nil || len(userBlocks.Blocks) == 0 {
		return
	}

	foundBlock := userBlocks.Blocks[0]
	if foundBlock.Block == account.LastBlockID {
		return
	}

	msg := fmt.Sprintf("💽 <b>%v</b> new block at %v <b>#%v</b> (%v SIGNA)",
		account.AccountRS, common.FormatChainTimeToStringTimeUTC(foundBlock.Timestamp), foundBlock.Height, foundBlock.BlockReward)

	account.DbAccount.LastBlockID = foundBlock.Block
	n.db.Save(&account.DbAccount)

	n.notifierCh <- NotifierMessage{
		UserName: account.UserName,
		ChatID:   account.ChatID,
		Message:  msg,
	}
}

func (n *Notifier) checkMessageTransactions(account *MonitoredAccount) {
	userMessages, err := n.signumClient.GetCachedAccountMessageTransaction(account.Account)
	if err != nil {
		log.Printf("Can't get last account %v message transactions: %v", account.Account, err)
		return
	}

	if userMessages == nil || len(userMessages.Transactions) == 0 {
		return
	}

	if userMessages.Transactions[0].TransactionID == account.LastMessageTX {
		return
	}

	for _, transaction := range userMessages.Transactions {
		if transaction.TransactionID == account.LastMessageTX {
			break
		}
		var incomeTransaction = transaction.Sender != account.Account

		msg := fmt.Sprintf("📝 <b>%v</b> ", account.AccountRS)

		switch transaction.Subtype {
		case signumapi.TST_ARBITRARY_MESSAGE:
			var message string
			if transaction.Attachment.MessageIsText && transaction.Attachment.Message != "" {
				message = transaction.Attachment.Message
			} else {
				message = "[encrypted]"
			}

			if incomeTransaction {
				var senderName string
				senderAccount, err := n.signumClient.GetCachedAccount(transaction.SenderRS)
				if err == nil && senderAccount.Name != "" {
					senderName = fmt.Sprintf("\n<i>Name:</i> %v", senderAccount.Name)
				}

				msg += fmt.Sprintf("new message received:"+
					"\n<i>Sender:</i> %v"+senderName+
					"\n<i>Message:</i> "+message+
					"\n<i>Fee:</i> %v SIGNA",
					transaction.SenderRS, transaction.FeeNQT/1e8)
			} else {
				var recipientName string
				recipientAccount, err := n.signumClient.GetCachedAccount(transaction.RecipientRS)
				if err == nil && recipientAccount.Name != "" {
					recipientName = fmt.Sprintf("\n<i>Name:</i> %v", recipientAccount.Name)
				}

				msg += fmt.Sprintf("new message sent:"+
					"\n<i>Recipient:</i> %v"+recipientName+
					"\n<i>Message:</i> "+message+
					"\n<i>Fee:</i> %v SIGNA",
					transaction.RecipientRS, transaction.FeeNQT/1e8)
			}
		default:
			log.Printf("%v: unknown SubType (%v) for transaction %v", account.Account, transaction.Subtype, transaction.TransactionID)
			continue
		}

		n.notifierCh <- NotifierMessage{
			UserName: account.UserName,
			ChatID:   account.ChatID,
			Message:  msg,
		}
	}

	account.DbAccount.LastMessageTX = userMessages.Transactions[0].TransactionID
	n.db.Save(&account.DbAccount)
}
