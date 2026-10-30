package admin

// TLS 证书库（HY2 用）：/admin-api/xray-certificates。
//
// 存在理由：hysteria 强制 TLS，而 xray_inbounds.stream_settings 是明文 JSON 列，
// 把 PEM 私钥塞进去等于私钥明文落库。所以入站的 tlsSettings 只存证书库 id
// （tlsCertId），私钥加密放 xray_certificates.key_enc，渲染时按 id 解密注入。
//
// 这里只做「证书库自身」的增删查：不碰入站引用计数（删证书时被引用的入站会在
// 渲染时拿不到 certificates，行级校验由前端提示 + 渲染结果暴露）。

import (
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
)

// xrayCertificateIndex 对齐 index()：返回证书库列表（私钥永不回显）。
func (d *NodeDomain) xrayCertificateIndex(c *gin.Context) {
	certs, err := d.store.XrayCertificates(ctxOf(c))
	if err != nil {
		d.failServer(c, "查询证书库", err)
		return
	}
	items := make(jsArr, 0, len(certs))
	for i := range certs {
		items = append(items, presentXrayCertificate(&certs[i]))
	}
	d.success(c, jo("certificates", items))
}

// xrayCertificateStore 对齐 store()：新增一条证书。
func (d *NodeDomain) xrayCertificateStore(c *gin.Context) {
	input := jsonInput(c)
	v := newNodeValidator(ctxOf(c), input)
	v.field("name", "required", "string", "max:190")
	v.field("domain", "nullable", "string", "max:190")
	v.field("cert_pem", "required", "string", "max:65535")
	v.field("key_pem", "required", "string", "max:65535")
	if !v.passes() {
		d.failNodeValidation(c, v)
		return
	}

	certPEM := strings.TrimSpace(nodeStringField(input, "cert_pem"))
	keyPEM := strings.TrimSpace(nodeStringField(input, "key_pem"))
	if !strings.Contains(certPEM, "BEGIN CERTIFICATE") {
		d.fail(c, "证书必须是 PEM 格式（以 -----BEGIN CERTIFICATE----- 开头）", 500)
		return
	}
	if !strings.Contains(keyPEM, "PRIVATE KEY") {
		d.fail(c, "私钥必须是 PEM 格式（含 PRIVATE KEY 段）", 500)
		return
	}
	// 名字唯一（对齐 xray_certificates_name_unique）：重复时给可读文案而不是 SQL 报错。
	existing, err := d.store.XrayCertificates(ctxOf(c))
	if err != nil {
		d.failServer(c, "查询证书库", err)
		return
	}
	name := strings.TrimSpace(nodeStringField(input, "name"))
	for i := range existing {
		if existing[i].Name == name {
			d.fail(c, "证书名称 "+name+" 已存在", 500)
			return
		}
	}

	in := XrayCertificateInput{
		Name:    name,
		CertPEM: certPEM,
		KeyPEM:  keyPEM,
	}
	if domain := strings.TrimSpace(nodeStringField(input, "domain")); domain != "" {
		in.Domain = &domain
	}
	created, err := d.store.CreateXrayCertificate(ctxOf(c), in)
	if err != nil {
		d.failServer(c, "保存证书", err)
		return
	}
	d.success(c, jo("certificate", presentXrayCertificate(created)))
}

// xrayCertificateDestroy 对齐 destroy()。
func (d *NodeDomain) xrayCertificateDestroy(c *gin.Context) {
	id, ok := parseUint(c.Param("certificate"))
	if !ok {
		d.notFound(c)
		return
	}
	if err := d.store.DeleteXrayCertificate(ctxOf(c), id); err != nil {
		d.failServer(c, "删除证书", err)
		return
	}
	d.success(c, jo("deleted", true))
}

// presentXrayCertificate 只输出元信息（私钥绝不回显，PEM 本体也不进列表载荷）。
func presentXrayCertificate(c *XrayCertificate) *jsObj {
	return jo(
		"id", c.ID,
		"name", c.Name,
		"domain", nullableStr(c.Domain),
		"has_cert", c.CertPEM != "",
		"has_key", c.KeyEnc != "",
		"cert_pem_chars", utf8.RuneCountInString(c.CertPEM),
		"not_after", nullableISO(c.NotAfter),
	)
}
