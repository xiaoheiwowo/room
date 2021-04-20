package commands

import (
	"bytepower_room/base"
	"bytepower_room/base/log"
	"errors"

	"github.com/go-redis/redis/v8"
)

type Transaction struct {
	tx          *redis.Tx
	watchedKeys []string
	keys        []string
	started     bool
	commands    []redis.Cmder
	closed      bool
}

func NewTransaction() *Transaction {
	return &Transaction{}
}

func newRedisTransaction(keys ...string) (*redis.Tx, error) {
	if len(keys) == 0 {
		return base.GetRedisCluster().NewTransation(contextTODO, "")
	}
	if !redis.AreKeysInSameSlot(keys...) {
		return nil, errors.New("ERR keys in transaction should be in the same slot")
	}
	return base.GetRedisCluster().NewTransation(contextTODO, keys[0])
}

func (transaction *Transaction) multi() RESPData {
	if transaction.started {
		return RESPData{DataType: ErrorRespType, Value: errors.New("ERR MULTI calls can not be nested")}
	}
	transaction.started = true
	return RESPData{DataType: SimpleStringRespType, Value: "OK"}
}

func (transaction *Transaction) reset() error {
	if transaction.tx != nil {
		if err := transaction.tx.Close(contextTODO); err != nil {
			return err
		}
		transaction.tx = nil
	}
	transaction.watchedKeys = make([]string, 0)
	transaction.keys = make([]string, 0)
	transaction.started = false
	transaction.commands = make([]redis.Cmder, 0)
	transaction.closed = false
	return nil
}

func (transaction *Transaction) watch(keys ...string) RESPData {
	if transaction.started {
		return RESPData{DataType: ErrorRespType, Value: errors.New("ERR WATCH inside MULTI is not allowed")}
	}
	if len(keys) == 0 {
		return convertErrorToRESPData(newWrongNumberOfArgumentsError("watch"))
	}

	if transaction.tx != nil {
		if len(transaction.watchedKeys) != 0 && !redis.AreKeysInSameSlot(append(transaction.watchedKeys, keys...)...) {
			if err := transaction.reset(); err != nil {
				return convertErrorToRESPData(err)
			}
		}
	}

	if transaction.tx == nil {
		tx, err := newRedisTransaction(keys...)
		if err != nil {
			return convertErrorToRESPData(err)
		}
		transaction.tx = tx
	}

	if _, err := transaction.tx.Watch(contextTODO, keys...).Result(); err != nil {
		return convertErrorToRESPData(err)
	}
	transaction.watchedKeys = append(transaction.watchedKeys, keys...)
	return RESPData{DataType: SimpleStringRespType, Value: "OK"}
}

func (transaction *Transaction) addCommand(command Commander) RESPData {
	var result RESPData
	if transaction.started {
		transaction.commands = append(transaction.commands, command.Cmd())
		transaction.keys = append(transaction.keys, append(command.ReadKeys(), command.WriteKeys()...)...)
		if (transaction.tx == nil) && len(transaction.keys) != 0 {
			tx, err := newRedisTransaction(transaction.keys...)
			if err != nil {
				return convertErrorToRESPData(err)
			}
			transaction.tx = tx
		}
		result = RESPData{DataType: SimpleStringRespType, Value: "QUEUED"}
	} else {
		if command.Name() == "unwatch" {
			result = transaction.unwatch()
		} else {
			result = ExecuteCommand(command)
		}
	}
	return result
}

func (transaction *Transaction) exec() RESPData {
	if !transaction.started {
		return convertErrorToRESPData(errors.New("ERR EXEC without MULTI"))
	}
	defer func() {
		if err := transaction.Close(); err != nil {
			logger := base.GetServerLogger()
			logger.Error(
				"close transaction error",
				log.String("command", "exec"),
				log.Error(err),
			)
		}
	}()
	if !redis.AreKeysInSameSlot(transaction.keys...) {
		return convertErrorToRESPData(errors.New("ERR keys in transaction should be in the same slot"))
	}
	if len(transaction.watchedKeys) != 0 && !redis.AreKeysInSameSlot(append(transaction.keys, transaction.watchedKeys...)...) {
		if transaction.tx != nil {
			if err := transaction.tx.Close(contextTODO); err != nil {
				logger := base.GetServerLogger()
				logger.Error(
					"close redis transaction error",
					log.String("command", "exec"),
					log.Error(err),
				)
			}
			transaction.tx = nil
			transaction.watchedKeys = make([]string, 0)
		}
	}

	if transaction.tx == nil {
		tx, err := newRedisTransaction(transaction.keys...)
		if err != nil {
			return convertErrorToRESPData(err)
		}
		transaction.tx = tx
	}

	pipeline := transaction.tx.TxPipeline()
	for _, cmd := range transaction.commands {
		if err := pipeline.Process(contextTODO, cmd); err != nil {
			return convertErrorToRESPData(err)
		}
	}

	commands, err := pipeline.Exec(contextTODO)
	if err != nil {
		return convertErrorToRESPData(err)
	}

	result := RESPData{DataType: ArrayRespType}
	value := make([]RESPData, 0)
	for _, command := range commands {
		r := convertCmdResultToRESPData(command)
		value = append(value, r)
	}
	result.Value = value
	return result
}

func (transaction *Transaction) Close() error {
	transaction.closed = true
	if transaction.tx != nil {
		if err := transaction.tx.Close(contextTODO); err != nil {
			return err
		}
		transaction.tx = nil
	}
	transaction.watchedKeys = make([]string, 0)
	transaction.keys = make([]string, 0)
	transaction.commands = make([]redis.Cmder, 0)
	return nil
}

func (transaction *Transaction) IsClosed() bool {
	return transaction.closed
}

func (transaction *Transaction) discard() RESPData {
	if !transaction.started {
		return convertErrorToRESPData(errors.New("ERR DISCARD without MULTI"))
	}
	if err := transaction.Close(); err != nil {
		logger := base.GetServerLogger()
		logger.Error(
			"close transaction error",
			log.String("command", "discard"),
			log.Error(err),
		)
	}
	return RESPData{DataType: SimpleStringRespType, Value: "OK"}
}

func (transaction *Transaction) unwatch() RESPData {
	if transaction.tx != nil {
		if _, err := transaction.tx.Unwatch(contextTODO).Result(); err != nil {
			return convertErrorToRESPData(err)
		}
		transaction.watchedKeys = make([]string, 0)
	}
	return RESPData{DataType: SimpleStringRespType, Value: "OK"}
}

func (transaction *Transaction) Process(command Commander) RESPData {
	var result RESPData
	if command.Name() == "watch" {
		result = transaction.watch(command.ReadKeys()...)
	} else if command.Name() == "multi" {
		result = transaction.multi()
	} else if command.Name() == "exec" {
		result = transaction.exec()
	} else if command.Name() == "discard" {
		result = transaction.discard()
	} else {
		result = transaction.addCommand(command)
	}
	return result
}

type WatchCommand struct {
	keys []string
	commonCommand
}

func NewWatchCommand(args []string) (Commander, error) {
	command := &WatchCommand{}
	command.init(args)
	if len(args) < 2 {
		return nil, newWrongNumberOfArgumentsError(command.name)
	}
	command.keys = args[1:]
	return command, nil
}

func (command *WatchCommand) Cmd() redis.Cmder {
	return redis.NewStatusCmd(contextTODO, command.argsToInterfaceSlice()...)
}

func (command *WatchCommand) ReadKeys() []string {
	return command.keys
}

func (command *WatchCommand) WriteKeys() []string {
	return []string{}
}

type MultiCommand struct {
	commonCommand
}

func NewMultiCommand(args []string) (Commander, error) {
	command := &MultiCommand{}
	command.init(args)
	if len(args) != 1 {
		return nil, newWrongNumberOfArgumentsError(command.name)
	}
	return command, nil
}

func (command *MultiCommand) Cmd() redis.Cmder {
	return redis.NewStatusCmd(contextTODO, command.name)
}

type ExecCommand struct {
	commonCommand
}

func NewExecCommand(args []string) (Commander, error) {
	command := &ExecCommand{}
	command.init(args)
	if len(args) != 1 {
		return nil, newWrongNumberOfArgumentsError(command.name)
	}
	return command, nil
}

func (command *ExecCommand) Cmd() redis.Cmder {
	return redis.NewCmd(contextTODO, command.name)
}

type DiscardCommand struct {
	commonCommand
}

func NewDiscardCommand(args []string) (Commander, error) {
	command := &DiscardCommand{}
	command.init(args)
	if len(args) != 1 {
		return nil, newWrongNumberOfArgumentsError(command.name)
	}
	return command, nil
}

func (command *DiscardCommand) Cmd() redis.Cmder {
	return redis.NewStatusCmd(contextTODO, command.name)
}

type UnwatchCommand struct {
	commonCommand
}

func NewUnwatchCommand(args []string) (Commander, error) {
	command := &UnwatchCommand{}
	command.init(args)
	if len(args) != 1 {
		return nil, newWrongNumberOfArgumentsError(command.name)
	}
	return command, nil
}

func (command *UnwatchCommand) Cmd() redis.Cmder {
	return redis.NewStatusCmd(contextTODO, command.name)
}
