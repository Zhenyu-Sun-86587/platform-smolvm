use strict;
use warnings;
use Digest::SHA qw(sha256);
use IO::Socket::INET;
use JSON::PP qw(encode_json);
use Socket qw(IPPROTO_TCP TCP_NODELAY inet_aton inet_ntoa);
use Time::HiRes qw(clock_gettime CLOCK_MONOTONIC);

my ($mode, $port, $destination, $expected_peer) = @ARGV;
my $frame_bytes = 32;
my $warmup = 4;
my $rtt_samples = 48;
my $bulk_samples = 3;
my $block_bytes = 64 * 1024;
my $bulk_bytes = 4 * 1024 * 1024;

sub read_exact {
    my ($socket, $length) = @_;
    my $data = '';
    while (length($data) < $length) {
        my $remaining = $length - length($data);
        my $read = sysread($socket, $data, $remaining, length($data));
        die "socket read: $!" unless defined $read;
        die 'unexpected EOF' unless $read;
    }
    return $data;
}

sub write_all {
    my ($socket, $data) = @_;
    my $offset = 0;
    while ($offset < length($data)) {
        my $written = syswrite($socket, $data, length($data) - $offset, $offset);
        die "socket write: $!" unless $written;
        $offset += $written;
    }
}

sub no_delay {
    my ($socket) = @_;
    setsockopt($socket, IPPROTO_TCP, TCP_NODELAY, pack('i', 1)) or die "TCP_NODELAY: $!";
}

sub connect_case {
    my ($operation, $size) = @_;
    my $socket = IO::Socket::INET->new(PeerAddr => $destination, PeerPort => $port, Proto => 'tcp', Timeout => 8)
        or die "connect $destination: $!";
    no_delay($socket);
    write_all($socket, pack('CN', $operation, $size));
    my $peer = inet_ntoa(read_exact($socket, 4));
    die "peer saw $peer, expected $expected_peer" unless $peer eq $expected_peer;
    return $socket;
}

sub serve {
    my $listener = IO::Socket::INET->new(LocalAddr => '0.0.0.0', LocalPort => $port,
                                        Proto => 'tcp', Listen => 4, ReuseAddr => 1) or die "listen: $!";
    while (my $socket = $listener->accept()) {
        alarm 18;
        no_delay($socket);
        my ($operation, $size) = unpack('CN', read_exact($socket, 5));
        write_all($socket, inet_aton($socket->peerhost()));
        if ($operation == 1 && $size == $warmup + $rtt_samples) {
            for (1 .. $size) {
                write_all($socket, read_exact($socket, $frame_bytes));
            }
        } elsif ($operation == 2 && $size == $bulk_bytes) {
            my $digest = Digest::SHA->new(256);
            for (1 .. $bulk_bytes / $block_bytes) {
                $digest->add(read_exact($socket, $block_bytes));
            }
            write_all($socket, $digest->digest());
        } else {
            die 'unknown fixture operation or payload size';
        }
        close($socket);
        alarm 0;
    }
}

sub echo_short_frames {
    my $socket = connect_case(1, $warmup + $rtt_samples);
    my @samples;
    for my $index (0 .. $warmup + $rtt_samples - 1) {
        my $frame = pack('N', $index) . ('r' x ($frame_bytes - 4));
        my $start = clock_gettime(CLOCK_MONOTONIC);
        write_all($socket, $frame);
        my $echo = read_exact($socket, $frame_bytes);
        my $duration = clock_gettime(CLOCK_MONOTONIC) - $start;
        die 'echo differed from the transmitted frame' unless $echo eq $frame;
        push @samples, int($duration * 1e9) if $index >= $warmup;
    }
    return {frame_bytes => $frame_bytes, warmup => $warmup, rtt_ns => \@samples};
}

sub send_bulk_payloads {
    my @samples;
    for my $index (0 .. $bulk_samples - 1) {
        my $payload = '';
        for my $block (0 .. $bulk_bytes / $block_bytes - 1) {
            $payload .= pack('NN', $index, $block) . ('b' x ($block_bytes - 8));
        }
        my $expected = sha256($payload);
        my $socket = connect_case(2, $bulk_bytes);
        my $start = clock_gettime(CLOCK_MONOTONIC);
        write_all($socket, $payload);
        my $digest = read_exact($socket, 32);
        my $duration = clock_gettime(CLOCK_MONOTONIC) - $start;
        die 'receiver digest differed from the transmitted payload' unless $digest eq $expected;
        push @samples, {bytes => $bulk_bytes, elapsed_ns => int($duration * 1e9), sha256 => unpack('H*', $digest)};
        close($socket);
    }
    return {transfers => \@samples};
}

local $SIG{ALRM} = sub { die "private network fixture timed out\n"; };
if ($mode eq 'server') {
    serve();
    exit;
}
alarm 40;
my $result;
if ($mode eq 'rtt') {
    $result = echo_short_frames();
} elsif ($mode eq 'bulk') {
    $result = send_bulk_payloads();
} else {
    die 'unknown fixture mode';
}
print encode_json($result), "\n";
