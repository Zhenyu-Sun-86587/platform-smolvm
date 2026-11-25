use strict;
use warnings;
use IO::Socket::INET;

my $listener = IO::Socket::INET->new(
    LocalAddr => '0.0.0.0',
    LocalPort => 8080,
    Listen    => 8,
    ReuseAddr => 1,
) or die "Cannot listen on port 8080: $!";

my $body = "Sandbox workspace ready\n";
while (my $client = $listener->accept()) {
    binmode $client, ':raw';
    my $request = <$client>;
    if (defined $request) {
        while (defined(my $line = <$client>)) {
            last if $line eq "\r\n" || $line eq "\n";
        }
        print {$client} "HTTP/1.1 200 OK\r\n",
            "Content-Type: text/plain\r\n",
            "Content-Length: ", length($body), "\r\n",
            "Connection: close\r\n\r\n", $body;
    }
    close $client;
}
