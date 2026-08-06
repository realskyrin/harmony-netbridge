import dnsMessageTest from './DnsMessage.test';
import hnbProtocolTest from './HnbProtocol.test';
import reconnectPolicyTest from './ReconnectPolicy.test';

export default function testsuite(): void {
  dnsMessageTest();
  hnbProtocolTest();
  reconnectPolicyTest();
}
