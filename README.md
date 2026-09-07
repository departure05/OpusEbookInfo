`OpusEbookInfo` is a script add-in for Directory Opus that lets you access all existing metadata from your ebooks, directly in columns that you can customize down to the last detail.

### Features

* Native support for .epub, .pdf, .cbr, .cbz, .cb7 and .cbt. Also, you can add any extensions you want.
* Native support for sidecar .opf files.
* Blazing fast!
* Unified names for columns so you can enable them and have them work across many formats.
* Easy to use. Just open the config dialog with a file, or drop a file in, pick the properties you want to add by their preview value, and you're good to go.
* A built-in UI to manage which columns you want, with many customization options.
* Real-time preview of values in each property, as they'd appear in the file display.
* Support for custom columns that can pull their value from multiple fields into a single column.
* Easily switch data types on the fly.
* Support for Calibre custom-defined columns.
* Easily import and export your configuration.
* Preview metadata via the exposed command.
* Optional support for previews and thumbnails in Opus from covers.

and more!

<img width="1065" height="789" alt="preview" src="https://github.com/user-attachments/assets/aef5b628-686a-4551-a158-d9a3c6a48ca2" />
<img width="1274" height="415" alt="infotip" src="https://github.com/user-attachments/assets/f1809a14-1d68-4d15-8c83-aede747db366" />


---

Do you want something like this, but using [MediaInfo](https://mediaarea.net/en/MediaInfo) instead? Check out [my other project](https://resource.dopus.com/t/opusmediainfo-all-the-mediainfo-data-you-can-ask-for/56344)!

Do you want something like this, but using [ExifTool](https://exiftool.org/) instead? Check out [my other project](https://resource.dopus.com/t/opusexiftool-all-the-exiftool-data-you-can-ask-for/58559)!

---

### **Installation**

[Download from here](https://github.com/departure05/OpusEbookInfo/releases/download/latest/OpusEbookInfo.opusscriptinstall)

Then install the script as usual.

[https://resource.dopus.com/t/how-to-use-buttons-and-scripts-from-this-forum/3546/2](https://resource.dopus.com/t/how-to-use-buttons-and-scripts-from-this-forum/3546/2)

> This script uses a Go executable as a helper.
> Whenever you use the script, it first checks whether the helper executable exists. If it doesn't, you'll be prompted to let the script create it for you.

---

### **Requirements**
* Directory Opus 13.24.3 or higher.
* Windows 10 1803 or higher (since the script uses Unix sockets for faster intercommunication with the helper).

---

### **Usage**

The first time you install the script, it'll ask if you want to start configuring columns.

To configure which columns are enabled, you need to open the configuration dialog, which you can do in a few different ways:

* From the Script Management window, click the Configure button.
* Using the `OpusEbookInfo CONFIG` command. You can also include the full path to a file you'd like to preview.

<img width="1542" height="806" alt="config" src="https://github.com/user-attachments/assets/af319514-f594-4601-a545-ee96471ae902" />

Checked properties are the ones that will be enabled as columns in Opus once you close the dialog.

* The "Type" control sets the data type of the column's content. Allowed types are: String (text), number (int), double, star (rating), datetime, date, time, duration, list, and size.
You can change this value and immediately see how it affects the preview below.
* The "Category" control lets you change where the column appears. By default, columns are added under Script > OpusEbookInfo. You can switch this to any other category you prefer. This doesn't affect the behavior or output of the column.
* The "Pattern" controls let you apply advanced formatting using regular expressions.
By default, the script uses predefined formats to adjust the type of each value. You can override those with your own.
The first field is where you define the actual pattern to match. Use the same format as ECMAScript literal regex: `/pattern/flags`.
The second field defines the replacement string. Both follow the same format used in JScript's `String.replace(pattern, replace)`, so compatibility depends on that context.
* Some values (mostly in the OPF schema) can declare two values: a regular one and another to use for "sorting". You can choose to use either of the two.
* `Blur in secure screenshot` allows you to define which columns will appear blurred if Secure Screenshot is active.
* You can Export or Import columns from this same window, using the provided buttons.

Every time you open a file for preview, the script saves the listed properties for later use, so you don't have to remember which file had which properties when adding a new column.
After closing the dialog, the columns will be added automatically and will be ready to use in Opus, in any field where they apply.
Note: If the columns are already visible, you may need to refresh the lister to see the changes.

You can also access the Options dialog from here, which lets you configure things like the script log level, additional extensions, settings related to backups, and more.
(This dialog is also accessible from the Script Management window by clicking the About button).

<img width="876" height="584" alt="options" src="https://github.com/user-attachments/assets/64d0a382-e752-4b73-a55a-6d9e3238e01e" />


---

### **Custom Columns**

<img width="800" height="774" alt="custom" src="https://github.com/user-attachments/assets/0198a24b-193c-4b3f-9268-1729c0315d45" />


You can also create custom columns that pull values from multiple fields. Simply pick multiple fields and order them so the script takes the first value found and shows it as the column value. That way you can have a single column that groups several sources and adapts depending on the file. There are lots of possibilities.

These columns have the same customization options as the others (type, etc.). The only difference is that their value can depend on multiple fields instead of just one.

To create them, press the "Custom" button in the main UI. That opens a dialog that asks for the name (keyword) of the new column and lets you pick the fields to pull data from and define the processing order.

You can also edit existing custom columns using the "Edit..." button. (This only appears when a Custom column is selected).

---

### **Metadata and Covers**

##### In EPUB/.OPF sidecar

The script has full support for .opf versions 1/2/3, including custom values such as those added by Calibre.
If it registers an unsupported extension, it will search for a sidecar .opf file in:

* metadata.opf next to the file.
* filename.ext.opf next to the file.

Note that sidecars take precedence over everything else.

For covers, the preference rules are:

* Internal cover designated in the .opf file itself.
* External cover.jpg, relative to the file.

Additionally, it adds the following data:

* Table of Contents (number of registered entries).

##### In PDF

The script has full support for PDF metadata (including v2), supporting fields from both Dublin Core and the XMP format. Even if the file is encrypted!
Additionally, it adds the following data:

* Number of pages
* Encrypted?
* Various permissions
* Physical sheet dimensions (in points, cm, and inches).

For covers, the preference rules are:

* External cover.jpg, relative to the file.

##### In Comics

The script supports the most well-known formats (cbz, cbr, cb7, cbt) by parsing the ComicInfo.xml file. It also supports external ComicInfo.xml files relative to the file.

Additionally, it adds the following data:

* Number of pages (based on the number of images inside the archive).

For covers, the preference rules are:

* Internal cover designated in the ComicInfo.xml file.
* Internal cover.jpg, even if unspecific.
* External cover.jpg, relative to the file.
* Use of the first recognized numbered image inside the archive.

NOTE FOR COVERS: In the event that a cover is not found, the script uses an image named no_cover.jpg located next to the helper. This prevents Opus from constantly asking for the image. You can replace this image with one of your preference if you wish.

---

### **How To Configure Covers**

Currently, Opus allows you to override previews and thumbnails for any file through the use of **External Tools**.
The helper (goebook.exe) serves precisely this purpose.

Go to `Preferences / Miscellaneous / External Tools`:

You can use the code below as a template and add the extensions you need:

<ext_image_format desc="" ext="epub;cbz;cbr;cb7;cbt" flags="13" input=""/dopusdata/User Data/OpusEbookInfo/goebook.exe"  -COVER=%in% -OUT=%out_jpg%" name="ebooks" output="" thumbs="" />

Note that it is not recommended to override previews or thumbnails for PDFs, as there is no way to fall back to the rendered image that your registered PDF reader is likely generating.

---

### **Command's Arguments**

The command supports the following arguments:

| Argument | Type | Value | Desc |
| --- | --- | --- | --- |
| **CONFIG** | /O |  | Shows the dialog window for configuring columns. |
|  |  | *filepath* | Accepts a file path to populate the preview column right after starting. |
| **EXPORT** | /S |  | Exports columns to a file. |
| **IMPORT** | /O |  | Imports columns from a file without deleting existing ones. If no file is specified, you'll be prompted to choose one. |
| **!IMPORT** | /O |  | Imports columns from a file, deleting existing ones. If no file is specified, you'll be prompted to choose one. |
| **SHOW** | /K |  | Shows a preview for the desired file, including all metadata found and its cover (if any). |

---

### **Notes**

* The script can create the helper executable automatically (if you let it). Some antivirus software might consider this behavior suspicious, so please let me know if you encounter any false positives.
* Alternatively, you can compile the helper yourself using the included Go source code, then place the resulting executable in `/dopusdata/User Data/OpusEbookInfo/goebook.exe`. More details in `HOW_TO_COMPILE.md`.
* The helper runs a local server over a Unix socket for faster communication, so some firewall apps might require you to let Opus access the internet (why are you even blocking it already?).
* The helper automatically shuts down after 10 seconds of inactivity.
* For tighter control over values, fields known for having a way to refine their values (like contributors) are automatically split up by specialty.
* You might occasionally not see thumbnail changes for certain files due to the Opus thumbnail cache, which may have already saved an image for the file in question. To verify whether it is configured correctly, try viewing the file using Quickshow or the Preview Pane first. If you can see the desired cover there, the issue is caching.
The only way to fix that is to clear the entire thumbnail database.

---

### **Greetings**

* To the Opus team, as usual.

The GO helper use the following third party libraries:

* [PDFCPU](https://github.com/pdfcpu/pdfcpu)
* [archives](https://github.com/mholt/archives)

---

### **Changelog**

**v1.0 (Sep 06, 2026) :**

* Initial release.
