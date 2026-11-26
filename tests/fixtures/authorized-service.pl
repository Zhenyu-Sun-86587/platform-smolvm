use strict;
use warnings;
use Fcntl qw(O_WRONLY O_CREAT O_APPEND);
use IO::Socket::INET;

my ($mode, $port, $address, $request) = @ARGV;
my $kind = $mode =~ /udp$/ ? 'udp' : 'tcp';
my $receipts = '/run/authorized-service-receipts';

sub business_id {
    my ($data) = @_;
    return ($data // '') =~ /^([a-z0-9-]{1,64})\n?$/ ? $1 : undef;
}

sub record_receipt {
    my ($id) = @_;
    sysopen(my $output, $receipts, O_WRONLY | O_CREAT | O_APPEND, 0600) or die $!;
    my $line = "$id\n";
    syswrite($output, $line) == length($line) or die $!;
    close($output);
}

if ($mode =~ /^send-/) {
    die 'invalid fixture request' unless business_id($request);
    local $SIG{ALRM} = sub { die "service reply timed out\n" };
    alarm 40;
    my $client = IO::Socket::INET->new(Proto => $kind, PeerAddr => $address, PeerPort => $port) or die $!;
    if ($kind eq 'udp') {
        $client->send("$request\n") or die $!;
        $client->recv(my $reply, 256) or die $!;
        print $reply;
    } else {
        print {$client} "$request\n";
        print scalar(<$client>) // '';
    }
    exit;
}

my %local = (Proto => $kind, LocalAddr => '0.0.0.0', LocalPort => $port, ReuseAddr => 1);
if ($kind eq 'udp') {
    my $server = IO::Socket::INET->new(%local) or die $!;
    while (my $peer = $server->recv(my $data, 256)) {
        my $id = business_id($data);
        next unless defined $id;
        record_receipt($id);
        $server->send("$id\n", 0, $peer);
    }
    exit;
}

my $listener = IO::Socket::INET->new(%local, Listen => 8) or die $!;
while (my $client = $listener->accept()) {
    my $first = <$client>;
    my $id;
    if ($mode eq 'http') {
        $id = $1 if ($first // '') =~ /^POST \/receipt\/([a-z0-9-]{1,64}) HTTP\/1\.[01]\r?\n$/;
        while (my $line = <$client>) { last if $line =~ /^\r?\n$/; }
    } else {
        $id = business_id($first);
    }
    if (defined $id) {
        record_receipt($id);
        if ($id !~ /^close-/) {
            my $body = "$id\n";
            if ($mode eq 'http') {
                my $length = length($body);
                print {$client} "HTTP/1.1 200 OK\r\nContent-Length: $length\r\nConnection: close\r\n\r\n$body";
            } else {
                print {$client} $body;
            }
        }
    }
    close($client);
}
