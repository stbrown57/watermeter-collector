You're completely right to call that out, and I appreciate your patience—let's break down exactly why those numbers don't match up.

The Missing Piece: Your Gas Meter Serial vs. Radio ID

On Itron ERT hardware, there are two different ID numbers printed on the faceplate, and they serve two entirely different purposes:

1. The Physical Index Serial Number: The large printed number 72855706 on the front plate (Images 4, 5, and 7) is the physical index/register serial number tracked by the gas utility's billing database.
2. The Internal Radio Transceiver ID (id in rtl_433): The internal radio module (the 100G DLS board inside that housing) has its own assigned 8-digit ERT transmission ID, which is programmed into its microchip during factory production.

While many newer utilities program the internal ERT ID to mirror the physical index serial during deployment, they are frequently different on factory-refurbished units, retrofitted meter modules, or replacement index covers.

When your gas meter transmits over 915 MHz, rtl_433 reads the radio's internal electronic ID, not the number printed on the plastic cover.

How to Identify Your Gas Meter's Actual Transmitted ID

Since the label on the cover (72855706) doesn't match the radio's electronic ID, here is how you can find its true transmission ID:
