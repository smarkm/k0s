package server

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"text/template"

	config "github.com/Mirantis/mke/pkg/apis/v1beta1"
	"github.com/Mirantis/mke/pkg/certificate"
	"github.com/Mirantis/mke/pkg/constant"
	"github.com/Mirantis/mke/pkg/util"
	"github.com/pkg/errors"

	"github.com/sirupsen/logrus"
)

var (
	kubeconfigTemplate = template.Must(template.New("kubeconfig").Parse(`
apiVersion: v1
clusters:
- cluster:
    server: {{.URL}}
    certificate-authority-data: {{.CACert}}
  name: local
contexts:
- context:
    cluster: local
    namespace: default
    user: user
  name: Default
current-context: Default
kind: Config
preferences: {}
users:
- name: user
  user:
    client-certificate-data: {{.ClientCert}}
    client-key-data: {{.ClientKey}}
`))
)

type Certificates struct {
	CACert string

	CertManager certificate.Manager
	ClusterSpec *config.ClusterSpec
}

func (c *Certificates) Run() error {

	// Common CA
	if err := c.CertManager.EnsureCA("ca", "kubernetes-ca"); err != nil {
		return err
	}

	caCertPath, caCertKey := filepath.Join(constant.CertRoot, "ca.crt"), filepath.Join(constant.CertRoot, "ca.key")
	// We need CA cert loaded to generate client configs
	logrus.Debugf("CA key and cert exists, loading")
	cert, err := ioutil.ReadFile(caCertPath)
	if err != nil {
		return errors.Wrapf(err, "failed to read ca cert")
	}
	c.CACert = string(cert)

	// Front proxy CA
	if err := c.CertManager.EnsureCA("front-proxy-ca", "kubernetes-front-proxy-ca"); err != nil {
		return err
	}

	proxyCertPath, proxyCertKey := filepath.Join(constant.CertRoot, "front-proxy-ca.crt"), filepath.Join(constant.CertRoot, "front-proxy-ca.key")

	proxyClientReq := certificate.Request{
		Name:   "front-proxy-client",
		CN:     "front-proxy-client",
		O:      "front-proxy-client",
		CACert: proxyCertPath,
		CAKey:  proxyCertKey,
	}
	if _, err := c.CertManager.EnsureCertificate(proxyClientReq, constant.ApiserverUser); err != nil {
		return err
	}

	// admin cert & kubeconfig
	adminReq := certificate.Request{
		Name:   "admin",
		CN:     "kubernetes-admin",
		O:      "system:masters",
		CACert: caCertPath,
		CAKey:  caCertKey,
	}
	adminCert, err := c.CertManager.EnsureCertificate(adminReq, "root")
	if err != nil {
		return err
	}
	if err := kubeConfig(filepath.Join(constant.CertRoot, "admin.conf"), "https://localhost:6443", c.CACert, adminCert.Cert, adminCert.Key); err != nil {
		return err
	}

	if err := generateKeyPair("sa"); err != nil {
		return err
	}

	ccmReq := certificate.Request{
		Name:   "ccm",
		CN:     "system:kube-controller-manager",
		O:      "system:kube-controller-manager",
		CACert: caCertPath,
		CAKey:  caCertKey,
	}
	ccmCert, err := c.CertManager.EnsureCertificate(ccmReq, constant.ControllerManagerUser)
	if err != nil {
		return err
	}

	if err := kubeConfig(filepath.Join(constant.CertRoot, "ccm.conf"), "https://localhost:6443", c.CACert, ccmCert.Cert, ccmCert.Key); err != nil {
		return err
	}

	schedulerReq := certificate.Request{
		Name:   "scheduler",
		CN:     "system:kube-scheduler",
		O:      "system:kube-scheduler",
		CACert: caCertPath,
		CAKey:  caCertKey,
	}
	schedulerCert, err := c.CertManager.EnsureCertificate(schedulerReq, constant.SchedulerUser)
	if err != nil {
		return err
	}

	if err := kubeConfig(filepath.Join(constant.CertRoot, "scheduler.conf"), "https://localhost:6443", c.CACert, schedulerCert.Cert, schedulerCert.Key); err != nil {
		return err
	}

	kubeletClientReq := certificate.Request{
		Name:   "apiserver-kubelet-client",
		CN:     "apiserver-kubelet-client",
		O:      "system:masters",
		CACert: caCertPath,
		CAKey:  caCertKey,
	}
	if _, err := c.CertManager.EnsureCertificate(kubeletClientReq, constant.ApiserverUser); err != nil {
		return err
	}

	hostnames := []string{
		"kubernetes",
		"kubernetes.default",
		"kubernetes.default.svc",
		"kubernetes.default.svc.cluster",
		"kubernetes.svc.cluster.local",
		"127.0.0.1",
		"localhost",
	}

	hostnames = append(hostnames, c.ClusterSpec.API.Address)
	hostnames = append(hostnames, c.ClusterSpec.API.SANs...)

	internalAPIAddress, err := c.ClusterSpec.Network.InternalAPIAddress()
	if err != nil {
		return err
	}
	hostnames = append(hostnames, internalAPIAddress)

	serverReq := certificate.Request{
		Name:      "server",
		CN:        "kubernetes",
		O:         "kubernetes",
		CACert:    caCertPath,
		CAKey:     caCertKey,
		Hostnames: hostnames,
	}
	if _, err := c.CertManager.EnsureCertificate(serverReq, constant.ApiserverUser); err != nil {
		return err
	}

	mkeAPIReq := certificate.Request{
		Name:      "mke-api",
		CN:        "mke-api",
		O:         "kubernetes",
		CACert:    caCertPath,
		CAKey:     caCertKey,
		Hostnames: hostnames,
	}
	// TODO Not sure about the user...
	if _, err := c.CertManager.EnsureCertificate(mkeAPIReq, constant.ApiserverUser); err != nil {
		return err
	}

	return nil
}

func (c *Certificates) Stop() error {
	return nil
}

func kubeConfig(dest, url, caCert, clientCert, clientKey string) error {
	if util.FileExists(dest) {
		return nil
	}
	data := struct {
		URL        string
		CACert     string
		ClientCert string
		ClientKey  string
	}{
		URL:        url,
		CACert:     base64.StdEncoding.EncodeToString([]byte(caCert)),
		ClientCert: base64.StdEncoding.EncodeToString([]byte(clientCert)),
		ClientKey:  base64.StdEncoding.EncodeToString([]byte(clientKey)),
	}

	output, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer output.Close()

	return kubeconfigTemplate.Execute(output, &data)
}

func generateKeyPair(name string) error {
	keyFile := filepath.Join(constant.CertRoot, fmt.Sprintf("%s.key", name))
	pubFile := filepath.Join(constant.CertRoot, fmt.Sprintf("%s.pub", name))

	if util.FileExists(keyFile) && util.FileExists(pubFile) {
		return nil
	}

	reader := rand.Reader
	bitSize := 2048

	key, err := rsa.GenerateKey(reader, bitSize)
	if err != nil {
		return err
	}

	var privateKey = &pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	}

	outFile, err := os.Create(keyFile)
	if err != nil {
		return err
	}
	defer outFile.Close()

	err = pem.Encode(outFile, privateKey)

	// note to the next reader: key.Public() != key.PublicKey
	pubBytes, err := x509.MarshalPKIXPublicKey(key.Public())
	if err != nil {
		return err
	}

	var pemkey = &pem.Block{
		Type:  "PUBLIC KEY",
		Bytes: pubBytes,
	}

	pemfile, err := os.Create(pubFile)
	if err != nil {
		return err
	}
	defer pemfile.Close()

	err = pem.Encode(pemfile, pemkey)
	if err != nil {
		return err
	}

	return nil
}
