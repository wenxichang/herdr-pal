// Package adminauth 提供 Web 管理员摘要存储和内存会话安全机制。
package adminauth

import (
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

const (
	passwordMinBytes = 12
	passwordMaxBytes = 256
	argonMemoryKiB   = 64 * 1024
	argonIterations  = 3
	argonParallelism = 2
	argonSaltBytes   = 16
	argonKeyBytes    = 32
)

var (
	// ErrInvalidPassword 表示密码长度或编码不符合管理台安全要求。
	ErrInvalidPassword = errors.New("管理员密码无效")
	// ErrInvalidPasswordHash 表示认证文件中的 Argon2id PHC 字符串无效或参数不安全。
	ErrInvalidPasswordHash = errors.New("管理员密码摘要无效")
)

// Argon2idCodec 使用固定且有界的 Argon2id 参数生成和验证 PHC 密码摘要。
type Argon2idCodec struct{}

// NewArgon2idCodec 创建管理台固定参数的密码编解码器。
func NewArgon2idCodec() Argon2idCodec {
	return Argon2idCodec{}
}

// Hash 校验密码并使用调用方提供的安全随机源生成 Argon2id PHC 摘要。
func (Argon2idCodec) Hash(password string, random io.Reader) (string, error) {
	if err := validatePassword(password); err != nil {
		return "", err
	}
	if random == nil {
		return "", ErrInvalidPasswordHash
	}
	salt := make([]byte, argonSaltBytes)
	if _, err := io.ReadFull(random, salt); err != nil {
		return "", fmt.Errorf("%w: 读取密码盐", ErrInvalidPasswordHash)
	}
	digest := argon2.IDKey([]byte(password), salt, argonIterations, argonMemoryKiB, argonParallelism, argonKeyBytes)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version,
		argonMemoryKiB,
		argonIterations,
		argonParallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(digest),
	), nil
}

// Verify 解析有界 PHC 参数并使用常量时间比较验证密码。
func (Argon2idCodec) Verify(encoded, password string) (bool, error) {
	if err := validatePassword(password); err != nil {
		return false, nil
	}
	parameters, salt, want, err := parsePasswordHash(encoded)
	if err != nil {
		return false, err
	}
	got := argon2.IDKey([]byte(password), salt, parameters.iterations, parameters.memoryKiB, parameters.parallelism, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

type argonParameters struct {
	memoryKiB   uint32
	iterations  uint32
	parallelism uint8
}

func validatePassword(password string) error {
	if !utf8.ValidString(password) || len(password) < passwordMinBytes || len(password) > passwordMaxBytes {
		return ErrInvalidPassword
	}
	return nil
}

func parsePasswordHash(encoded string) (argonParameters, []byte, []byte, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" || parts[2] != "v="+strconv.Itoa(argon2.Version) {
		return argonParameters{}, nil, nil, ErrInvalidPasswordHash
	}
	parameterParts := strings.Split(parts[3], ",")
	if len(parameterParts) != 3 {
		return argonParameters{}, nil, nil, ErrInvalidPasswordHash
	}
	memory, err := parsePHCUnsigned(parameterParts[0], "m=", 32)
	if err != nil || memory != argonMemoryKiB {
		return argonParameters{}, nil, nil, ErrInvalidPasswordHash
	}
	iterations, err := parsePHCUnsigned(parameterParts[1], "t=", 32)
	if err != nil || iterations != argonIterations {
		return argonParameters{}, nil, nil, ErrInvalidPasswordHash
	}
	parallelism, err := parsePHCUnsigned(parameterParts[2], "p=", 8)
	if err != nil || parallelism != argonParallelism {
		return argonParameters{}, nil, nil, ErrInvalidPasswordHash
	}
	salt, err := base64.RawStdEncoding.Strict().DecodeString(parts[4])
	if err != nil || len(salt) != argonSaltBytes {
		return argonParameters{}, nil, nil, ErrInvalidPasswordHash
	}
	digest, err := base64.RawStdEncoding.Strict().DecodeString(parts[5])
	if err != nil || len(digest) != argonKeyBytes {
		return argonParameters{}, nil, nil, ErrInvalidPasswordHash
	}
	return argonParameters{memoryKiB: uint32(memory), iterations: uint32(iterations), parallelism: uint8(parallelism)}, salt, digest, nil
}

func parsePHCUnsigned(value, prefix string, bits int) (uint64, error) {
	if !strings.HasPrefix(value, prefix) || len(value) == len(prefix) {
		return 0, ErrInvalidPasswordHash
	}
	parsed, err := strconv.ParseUint(value[len(prefix):], 10, bits)
	if err != nil {
		return 0, ErrInvalidPasswordHash
	}
	return parsed, nil
}
