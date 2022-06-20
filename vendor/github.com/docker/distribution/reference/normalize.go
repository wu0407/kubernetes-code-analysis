package reference

import (
	"errors"
	"fmt"
	"strings"

	"github.com/docker/distribution/digestset"
	"github.com/opencontainers/go-digest"
)

var (
	legacyDefaultDomain = "index.docker.io"
	defaultDomain       = "docker.io"
	officialRepoName    = "library"
	defaultTag          = "latest"
)

// normalizedNamed represents a name which has been
// normalized and has a familiar form. A familiar name
// is what is used in Docker UI. An example normalized
// name is "docker.io/library/ubuntu" and corresponding
// familiar name of "ubuntu".
type normalizedNamed interface {
	Named
	Familiar() Named
}

// ParseNormalizedNamed parses a string into a named reference
// transforming a familiar name from Docker UI to a fully
// qualified reference. If the value may be an identifier
// use ParseAnyReference.
// 验证并解析镜像地址为Named对象
func ParseNormalizedNamed(s string) (Named, error) {
	// 验证s是否匹配正则表达式`([a-f0-9]{64})`，即是否为64位16进制，如果为64位16进制，则直接返回错误
	if ok := anchoredIdentifierRegexp.MatchString(s); ok {
		return nil, fmt.Errorf("invalid repository name (%s), cannot specify 64-byte hexadecimal strings", s)
	}
	// 使用"/"进行分隔，解析出域名部分和剩余部分，并兼容docker官方仓库
	domain, remainder := splitDockerDomain(s)
	var remoteName string
	if tagSep := strings.IndexRune(remainder, ':'); tagSep > -1 {
		// 包含tag，解析出仓库名
		remoteName = remainder[:tagSep]
	} else {
		// 不包含tag，则剩下所有字符串为仓库名
		remoteName = remainder
	}
	// 如果仓库名不是小写，则直接返回
	if strings.ToLower(remoteName) != remoteName {
		return nil, errors.New("invalid reference format: repository name must be lowercase")
	}

	// 解析镜像名返回相应的Reference对象
	ref, err := Parse(domain + "/" + remainder)
	if err != nil {
		return nil, err
	}
	named, isNamed := ref.(Named)
	if !isNamed {
		return nil, fmt.Errorf("reference %s has no name", ref.String())
	}
	return named, nil
}

// splitDockerDomain splits a repository name to domain and remotename string.
// If no valid domain is found, the default domain is used. Repository name
// needs to be already validated before.
// 将name使用"/"进行分隔，解析出域名部分和剩余部分，并兼容docker官方仓库
func splitDockerDomain(name string) (domain, remainder string) {
	i := strings.IndexRune(name, '/')
	// 不包含"/"，或包含"/"但是不包含"."或":"且"/"分隔的前半部分不为"localhost"，则域名为"docker.io"
	if i == -1 || (!strings.ContainsAny(name[:i], ".:") && name[:i] != "localhost") {
		domain, remainder = defaultDomain, name
	} else {
		// 否则"/"分隔的前半部分为domain，后半部分为remainder
		domain, remainder = name[:i], name[i+1:]
	}
	// 兼容"index.docker.io"将它转成"docker.io"
	if domain == legacyDefaultDomain {
		domain = defaultDomain
	}
	// 如果"docker.io"且剩余部分没有'/'，添加"library/"前缀，比如nginx:1.16，则remainder为"library/nginx:1.16"
	if domain == defaultDomain && !strings.ContainsRune(remainder, '/') {
		remainder = officialRepoName + "/" + remainder
	}
	return
}

// familiarizeName returns a shortened version of the name familiar
// to to the Docker UI. Familiar names have the default domain
// "docker.io" and "library/" repository prefix removed.
// For example, "docker.io/library/redis" will have the familiar
// name "redis" and "docker.io/dmcgowan/myapp" will be "dmcgowan/myapp".
// Returns a familiarized named only reference.
func familiarizeName(named namedRepository) repository {
	repo := repository{
		domain: named.Domain(),
		path:   named.Path(),
	}

	if repo.domain == defaultDomain {
		repo.domain = ""
		// Handle official repositories which have the pattern "library/<official repo name>"
		if split := strings.Split(repo.path, "/"); len(split) == 2 && split[0] == officialRepoName {
			repo.path = split[1]
		}
	}
	return repo
}

func (r reference) Familiar() Named {
	return reference{
		namedRepository: familiarizeName(r.namedRepository),
		tag:             r.tag,
		digest:          r.digest,
	}
}

func (r repository) Familiar() Named {
	return familiarizeName(r)
}

func (t taggedReference) Familiar() Named {
	return taggedReference{
		namedRepository: familiarizeName(t.namedRepository),
		tag:             t.tag,
	}
}

func (c canonicalReference) Familiar() Named {
	return canonicalReference{
		namedRepository: familiarizeName(c.namedRepository),
		digest:          c.digest,
	}
}

// TagNameOnly adds the default tag "latest" to a reference if it only has
// a repo name.
func TagNameOnly(ref Named) Named {
	if IsNameOnly(ref) {
		namedTagged, err := WithTag(ref, defaultTag)
		if err != nil {
			// Default tag must be valid, to create a NamedTagged
			// type with non-validated input the WithTag function
			// should be used instead
			panic(err)
		}
		return namedTagged
	}
	return ref
}

// ParseAnyReference parses a reference string as a possible identifier,
// full digest, or familiar name.
func ParseAnyReference(ref string) (Reference, error) {
	if ok := anchoredIdentifierRegexp.MatchString(ref); ok {
		return digestReference("sha256:" + ref), nil
	}
	if dgst, err := digest.Parse(ref); err == nil {
		return digestReference(dgst), nil
	}

	return ParseNormalizedNamed(ref)
}

// ParseAnyReferenceWithSet parses a reference string as a possible short
// identifier to be matched in a digest set, a full digest, or familiar name.
func ParseAnyReferenceWithSet(ref string, ds *digestset.Set) (Reference, error) {
	if ok := anchoredShortIdentifierRegexp.MatchString(ref); ok {
		dgst, err := ds.Lookup(ref)
		if err == nil {
			return digestReference(dgst), nil
		}
	} else {
		if dgst, err := digest.Parse(ref); err == nil {
			return digestReference(dgst), nil
		}
	}

	return ParseNormalizedNamed(ref)
}
