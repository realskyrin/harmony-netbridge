import dnsMessageTest from './DnsMessage.test';
import hnbProtocolTest from './HnbProtocol.test';
import reconnectPolicyTest from './ReconnectPolicy.test';
import proxyCertificateTest from './ProxyCertificate.test';
import proxyDiagnosticsTest from './ProxyDiagnostics.test';

export default function testsuite(): void {
  dnsMessageTest();
  hnbProtocolTest();
  reconnectPolicyTest();
  proxyCertificateTest();
  proxyDiagnosticsTest();
}
