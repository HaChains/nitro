package env

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/ethereum/go-ethereum/log"
	"github.com/offchainlabs/nitro/kclients/util/sig"
)

// pause
const (
	EnvTestOffset      = "TEST_OFFSET"
	EnvTestRedisHeight = "TEST_REDIS_HEIGHT"
	EnvETLAllowBehind  = "ETL_ALLOW_BEHIND"
	EnvETLMasterName   = "ETL_MASTER_NAME"
	EnvETLPassword     = "ETL_PASSWORD"
	EnvETLAddrs        = "ETL_ADDRS"
	EnvETLDB           = "ETL_DB"
	EnvETLChainName    = "ETL_CHAIN_NAME"
)

// trace cache
const (
	EnvTraceCacheKey      = "TRACE_CACHE_KEY"
	EnvTraceCacheEndpoint = "TRACE_CACHE_ENDPOINT"
	EnvTraceCacheDB       = "TRACE_CACHE_DB"
	EnvTraceCacheSize     = "TRACE_CACHE_SIZE"
	EnvTraceCacheChanSize = "TRACE_CACHE_CHAN_SIZE"
)

const (
	WrongInt   = -1024
	DefaultInt = 0
)

func LoadEnvInt64(v string) int64 {
	env := os.Getenv(v)
	if env != "" {
		i, err := strconv.ParseInt(env, 10, 64)
		if err != nil {
			sig.Int(fmt.Sprintf("failed to parse '%s' of value '%s'\n", v, env))
			return WrongInt
		}
		log.Info("### DEBUG ### Loaded ENV", v, i)
		return i
	}
	log.Info("### DEBUG ### Load ENV", "name", v, "use default value", 0)
	return DefaultInt
}

func LoadEnvInt(v string) int {
	return int(LoadEnvInt64(v))
}

func LoadEnvString(v string) string {
	env := os.Getenv(v)
	if env != "" {
		log.Info("### DEBUG ### Loaded ENV", v, env)
	} else {
		log.Info("### DEBUG ### Load ENV", "name", v, "use default value", env)
	}
	return env
}

func LoadEnvStringMute(v string) string {
	env := os.Getenv(v)
	return env
}

func LoadEnvStrings(v string) (strs []string) {
	env := os.Getenv(v)
	if len(env) == 0 {
		return
	}
	strs = strings.Split(env, ",")
	log.Info("### DEBUG ### Loaded ENV", v, strs)
	return strs
}
