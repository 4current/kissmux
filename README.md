# kissmux

As the About says, kissmux connects to a device file on one side and opens a TCP socket to support multiple KISS clients with bi-directional serialization. Here's what it does:

	•	Opens one KISS device (e.g. /dev/soundmodem0)
	•	Listens on TCP (default :8001)
	•	Broadcasts device→all clients
	•	Serializes all client→device writes (no interleaving)
	•	Drops slow clients instead of blocking RF

Disclosure: The initial version was generated with prompting by ChatGPT because I just wanted to get something working quickly.
