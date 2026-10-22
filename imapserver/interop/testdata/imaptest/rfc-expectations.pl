#!/usr/bin/perl
# Corrects imaptest script expectations that contradict normative RFC text.
#
# Each correction replaces one literal passage and fails the image build when
# the passage is missing, so an upstream change cannot silently skip it. The
# reasons are recorded in docs/INTEROP.md (#205); a correction belongs here only
# when the RFC requires what this server does.
use strict;
use warnings;

my $dir = shift or die "usage: rfc-expectations.pl TESTDIR\n";

sub correct {
    my ($file, $old, $new) = @_;
    my $path = "$dir/$file";
    open my $in, '<:raw', $path or die "$path: $!\n";
    my $text = do { local $/; <$in> };
    close $in;
    my $at = index $text, $old;
    die "$path: expectation to correct not found\n" if $at < 0;
    substr($text, $at, length $old) = $new;
    open my $out, '>:raw', $path or die "$path: $!\n";
    print {$out} $text;
    close $out;
}

# RFC 5465 section 5: "If one of MessageNew or MessageExpunge is specified, then
# both events MUST be specified. Otherwise, the server MUST respond with the
# tagged BAD response." The same holds for FlagChange without both. The script
# expected OK for each single event; the STATUS content it checked is kept,
# with valid registrations.
correct('notify',
    "ok notify set status (mailboxes \${mailbox}2 (MessageExpunge))\n"
  . "* status \${mailbox}2 (messages 1)\n"
  . "\n"
  . "ok notify set status (mailboxes \${mailbox}2 (MessageNew))\n"
  . "* status \${mailbox}2 (messages 1 uidnext \$uidnext uidvalidity \$uidvalidity)\n"
  . "\n"
  . "ok notify set status (mailboxes \${mailbox}2 (FlagChange))\n"
  . "* status \${mailbox}2 (uidvalidity \$uidvalidity highestmodseq \$hmodseq)\n",
    "bad notify set status (mailboxes \${mailbox}2 (MessageExpunge))\n"
  . "bad notify set status (mailboxes \${mailbox}2 (MessageNew))\n"
  . "bad notify set status (mailboxes \${mailbox}2 (FlagChange))\n"
  . "\n"
  . "ok notify set status (mailboxes \${mailbox}2 (MessageNew MessageExpunge))\n"
  . "* status \${mailbox}2 (messages 1 uidnext \$uidnext uidvalidity \$uidvalidity)\n"
  . "\n"
  . "ok notify set status (mailboxes \${mailbox}2 (MessageNew MessageExpunge FlagChange))\n"
  . "* status \${mailbox}2 (messages 1 uidnext \$uidnext uidvalidity \$uidvalidity highestmodseq \$hmodseq)\n");

# RFC 9051 section 7.5.2 (BINARY): "servers MUST ensure that textual
# line-oriented sections are always transmitted using the IMAP CRLF line
# termination syntax". Part 5 is text/x-myown and decodes to
# "abcdefg\rhijkl\r\nmno\npqrstuvqxyz"; its lone CR and LF are line ends, so
# the section is 32 octets "abcdefg\r\nhijkl\r\nmno\r\npqrstuvqxyz", not 30.
# A {{{ block drops the newline before its closing braces.
my $file = 'fetch-binary-mime-base64';
correct($file, "binary.size[5] 30)", "binary.size[5] 32)");
correct($file,
    "binary[5] ~{{{\nabcdefg\rhijkl\r\nmno\npqrstuvqxyz\n}}})",
    "binary[5] ~{{{\nabcdefg\r\nhijkl\r\nmno\r\npqrstuvqxyz\n}}})");
correct($file,
    "binary[5]<0> ~{{{\nabcdefg\rhi\n}}})",
    "binary[5]<0> ~{{{\nabcdefg\r\nh\n}}})");
correct($file,
    "binary[5]<10> ~{{{\njkl\r\nmno\np\n}}})",
    "binary[5]<10> ~{{{\nijkl\r\nmno\r\n}}})");
correct($file,
    "binary[5]<5> ~{{{\nfg\rhijkl\r\n\n}}})",
    "binary[5]<5> ~{{{\nfg\r\nhijkl\r\n}}})");
correct($file,
    "binary[5]<15> ~{{{\nmno\npqrstuvqxyz\n}}})",
    "binary[5]<15> ~{{{\n\nmno\r\npqrstuvqxyz\n}}})");
