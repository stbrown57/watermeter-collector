You are in an interesting position. Henrico County explicitly uses Badger Recordall meters equipped with Itron-compatible radio modules or Badger ORION endpoints. [1] (https://henrico.gov/pdfs/purchasing/contracts/1840A/1840A_Contract_Extract_6-1-21_5-31-22.pdf), [2] (https://community.home-assistant.io/t/quick-setup-of-badger-orion-water-meter/933124), [3] (https://www.reddit.com/r/homeassistant/comments/1b07gdt/badger_orion_water_meter/)
The good news is that the transmission is highly unlikely to be encrypted. Per Henrico County's Department of Public Utilities (DPU), their Automated Meter Reading (AMR) signals strictly broadcast only the meter's raw serial number and current consumption index. They do not embed personal information or account data, which means they rely on "security through obscurity" rather than radio-level cryptographic encryption. [1] (https://henrico.gov/utility/faqs/)
However, because it's a recent construction using the Badger ORION system, you will likely need to use rtl_433 instead of rtlamr. Standard Itron ERT meters use rtlamr, but Badger ORION endpoints use a slightly different framing structure that is handled natively by rtl_433 via Protocol 219 or 223. [1] (https://gist.github.com/nickovs/f2e20f0352b146eb1daf5c40ef25be35), [2] (https://www.reddit.com/r/homeassistant/comments/1b07gdt/badger_orion_water_meter/), [3] (https://community.home-assistant.io/t/quick-setup-of-badger-orion-water-meter/933124)

Prerequisites for your Raspberry Pi

1. Ensure your RTL-SDR dongle is plugged into a USB 2.0 or 3.0 port on your Pi.
2. For 900 MHz reception through a pit lid (since water meters are usually buried in the ground in Henrico), a magnetic-mount whip antenna stuck to a ground plane (like a metal baking sheet) placed near a window facing the street is ideal.

Step-by-Step Setup on Linux


1. Install Dependencies and rtl_433

Open your Raspberry Pi terminal and install the required tools to compile or install rtl_433:
bash
sudo apt update
sudo apt install -y rtl-sdr librtlsdr-dev build-essential cmake git
Use code with caution.

2. Scan for the Meter

Depending on the exact version of the Badger ORION endpoint deployed at your property, it will either hop around 916.45 MHz or transmit in bursts across the wider 902–928 MHz ISM spectrum. [1] (https://community.home-assistant.io/t/quick-setup-of-badger-orion-water-meter/933124), [2] (https://gist.github.com/nickovs/f2e20f0352b146eb1daf5c40ef25be35), [3] (https://www.reddit.com/r/homeassistant/comments/1b07gdt/badger_orion_water_meter/), [4] (https://metertechsolutions.com/product/me/)
Run rtl_433 in discovery mode targeting the Badger protocols. Open a terminal and run:
bash
rtl_433 -f 916.45M -s 1200k -R 219 -R 223
Use code with caution.
Note: If you don't see anything immediately, remove the protocol flags (-R) and let it listen broadly to the frequency:
bash
rtl_433 -f 916.45M -s 1200k
Use code with caution.

3. Handling the "Frequency Hopping" Catch

One characteristic quirk noted by users targeting Badger Orion Endpoints is that they use a frequency-hopping spread spectrum. If you aren't picking up messages reliably on a fixed frequency, you can configure rtl_433 to hop across the low, mid, and high 900 MHz spectrum bands or log raw data. [1] (https://nick.blog/2026/02/16/reading-badger-water-meter-data-for-home-assistant/), [2] (https://www.reddit.com/r/homeassistant/comments/1b07gdt/badger_orion_water_meter/)
If it successfully picks up a reading, it will print out a JSON object to your terminal showing id (the meter number stamped on your pit cap) and volume_gal (the current meter reading in gallons). [1] (https://community.home-assistant.io/t/quick-setup-of-badger-orion-water-meter/933124)
When you run the base rtl_433 command, do you see any background chatter from nearby meters, or is the terminal completely quiet? If you receive error codes about kernel drivers (dvb_usb_rtl28xxu), let me know so I can give you the blacklisting steps.

