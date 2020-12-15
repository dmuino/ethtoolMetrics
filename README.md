# Generate ethtool -S metrics

Adds `ethtool -S` metrics to atlas using spectatord

```
  ethmetrics [options] where options are:

  -address string
    	hostname:port where spectatord is listening (default "127.0.0.1:1234")
  -frequency duration
    	Collect metrics at this frequency (default 30s)
  -ifaces string
    	Comma separated list of interfaces to query (default eth0)

```
